/*
 * VOXMail baresip boundary.
 *
 * This deliberately keeps SIP/media ownership inside baresip. The Go process
 * receives lifecycle/DTMF events over a Unix socket. Each call gets a private
 * PCM FIFO for Go-to-SIP audio and a raw PCM capture file for SIP-to-Go audio.
 * The media callbacks never call into Go or wait on the control socket.
 */
#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <re.h>
#include <signal.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <sys/stat.h>
#include <stdint.h>
#include <unistd.h>
#include <baresip.h>
#include <rem.h>

enum { SOCKET_PATH_MAX = 108, EVENT_BUFFER = 1024, AUDIO_PATH_MAX = 256 };

struct session {
	struct le le;
	struct call *call;
	char id[128];
	bool closed;
};

static struct list sessions;
static pthread_t socket_thread;
static volatile sig_atomic_t running;
static int server_fd = -1;
static int client_fd = -1;
static pthread_mutex_t client_lock = PTHREAD_MUTEX_INITIALIZER;
static pthread_mutex_t session_lock = PTHREAD_MUTEX_INITIALIZER;
static char socket_path[SOCKET_PATH_MAX];
static char audio_root[AUDIO_PATH_MAX] = "/data/run/voxmail";
static char audio_cur_id[AUDIO_PATH_MAX] = "default";

struct pcm_source {
	uint32_t ptime;
	size_t sampc;
	int fd;
	RE_ATOMIC bool run;
	thrd_t thread;
	struct ausrc_prm prm;
	ausrc_read_h *rh;
	void *arg;
};

struct pcm_player {
	int fd;
	char device[AUDIO_PATH_MAX];
	struct auplay_prm prm;
	auplay_write_h *wh;
	void *arg;
	size_t sampc;
	RE_ATOMIC bool run;
	thrd_t thread;
};

static struct ausrc *pcm_ausrc;
static struct auplay *pcm_auplay;

static int dial_pipe[2] = { -1, -1 };
static struct re_fhs *dial_fhs;

static void pcm_path(char *path, size_t sz, const char *device, const char *suffix)
{
	const char *id = str_isset(device) ? device : "default";
	while (*id == ',') ++id;
	re_snprintf(path, sz, "%s/%s.%s", audio_root, id, suffix);
}

static void pcm_source_destructor(void *arg)
{
	struct pcm_source *st = arg;
	if (re_atomic_rlx(&st->run)) {
		re_atomic_rlx_set(&st->run, false);
		thrd_join(st->thread, NULL);
	}
	if (st->fd >= 0)
		close(st->fd);
}

static int pcm_source_thread(void *arg)
{
	struct pcm_source *st = arg;
	size_t bytes = st->sampc * aufmt_sample_size(st->prm.fmt);
	void *samples = mem_zalloc(bytes, NULL);
	if (!samples)
		return ENOMEM;
	while (re_atomic_rlx(&st->run)) {
		struct auframe af;
		ssize_t got = 0;
		memset(samples, 0, bytes);
		if (st->fd >= 0)
			got = read(st->fd, samples, bytes);
		if (got < 0 && errno != EAGAIN && errno != EINTR)
			break;
		auframe_init(&af, st->prm.fmt, samples, st->sampc,
		             st->prm.srate, st->prm.ch);
		st->rh(&af, st->arg);
		sys_msleep(st->ptime ? st->ptime : 20);
	}
	mem_deref(samples);
	return 0;
}

static int pcm_source_alloc(struct ausrc_st **stp, const struct ausrc *as,
				struct ausrc_prm *prm, const char *device,
				ausrc_read_h *rh, ausrc_error_h *errh, void *arg)
{
	struct pcm_source *st;
	char path[AUDIO_PATH_MAX];
	int err;
	(void)as;
	(void)errh;
	if (!stp || !prm || !rh || prm->fmt != AUFMT_S16LE)
		return EINVAL;
	st = mem_zalloc(sizeof(*st), pcm_source_destructor);
	if (!st)
		return ENOMEM;
	st->fd = -1;
	st->prm = *prm;
	st->ptime = prm->ptime ? prm->ptime : 20;
	st->sampc = prm->srate * prm->ch * st->ptime / 1000;
	st->rh = rh;
	st->arg = arg;
	pcm_path(path, sizeof(path), str_isset(device) ? device : audio_cur_id, "tx.pcm");
	st->fd = open(path, O_RDONLY | O_NONBLOCK);
	if (st->fd < 0) {
		err = errno;
		mem_deref(st);
		return err;
	}
	re_atomic_rlx_set(&st->run, true);
	err = thread_create_name(&st->thread, "voxmail_pcm_in", pcm_source_thread, st);
	if (err) {
		mem_deref(st);
		return err;
	}
	*stp = (struct ausrc_st *)st;
	return 0;
}

static void pcm_player_destructor(void *arg)
{
	struct pcm_player *st = arg;
	if (re_atomic_rlx(&st->run)) {
		re_atomic_rlx_set(&st->run, false);
		thrd_join(st->thread, NULL);
	}
	if (st->fd >= 0)
		close(st->fd);
}

static int pcm_player_thread(void *arg)
{
	struct pcm_player *st = arg;
	size_t bytes = st->sampc * aufmt_sample_size(st->prm.fmt);
	void *samples = mem_zalloc(bytes, NULL);
	if (!samples)
		return ENOMEM;
	while (re_atomic_rlx(&st->run)) {
		struct auframe af;
		memset(samples, 0, bytes);
		auframe_init(&af, st->prm.fmt, samples, st->sampc,
		             st->prm.srate, st->prm.ch);
		st->wh(&af, st->arg);
		if (st->fd < 0) {
			char path[AUDIO_PATH_MAX];
			pcm_path(path, sizeof(path), st->device, "rx.pcm");
			st->fd = open(path, O_WRONLY | O_NONBLOCK | O_APPEND, 0600);
		}
		if (st->fd >= 0) {
			ssize_t wr = write(st->fd, samples, bytes);
			(void)wr;
			if (wr < 0 && (errno == EPIPE || errno == ENXIO)) {
				close(st->fd);
				st->fd = -1;
			}
		}
		sys_msleep(st->prm.ptime ? st->prm.ptime : 20);
	}
	mem_deref(samples);
	return 0;
}

static int pcm_player_alloc(struct auplay_st **stp, const struct auplay *ap,
				struct auplay_prm *prm, const char *device,
				auplay_write_h *wh, void *arg)
{
	struct pcm_player *st;
	char path[AUDIO_PATH_MAX];
	(void)ap;
	if (!stp || !prm || !wh || prm->fmt != AUFMT_S16LE)
		return EINVAL;
	st = mem_zalloc(sizeof(*st), pcm_player_destructor);
	if (!st)
		return ENOMEM;
	st->fd = -1;
	st->prm = *prm;
	st->device[0] = '\0';
	re_snprintf(st->device, sizeof(st->device), "%s",
	             str_isset(device) ? device : audio_cur_id);
	st->wh = wh;
	st->arg = arg;
	st->sampc = prm->srate * prm->ch * (prm->ptime ? prm->ptime : 20) / 1000;
	pcm_path(path, sizeof(path), st->device, "rx.pcm");
	st->fd = open(path, O_WRONLY | O_NONBLOCK | O_CREAT | O_APPEND, 0600);
	if (st->fd < 0 && errno == ENOENT) {
		(void)mkfifo(path, 0600);
		st->fd = open(path, O_WRONLY | O_NONBLOCK | O_APPEND, 0600);
	}
	if (st->fd < 0 && errno != ENXIO) {
		int err = errno;
		mem_deref(st);
		return err;
	}
	re_atomic_rlx_set(&st->run, true);
	int err = thread_create_name(&st->thread, "voxmail_pcm_out", pcm_player_thread, st);
	if (err) {
		mem_deref(st);
		return err;
	}
	*stp = (struct auplay_st *)st;
	return 0;
}

static void session_destructor(void *arg)
{
	struct session *session = arg;
	char path[AUDIO_PATH_MAX];
	pthread_mutex_lock(&session_lock);
	list_unlink(&session->le);
	pthread_mutex_unlock(&session_lock);
	pcm_path(path, sizeof(path), session->id, "tx.pcm");
	(void)unlink(path);
	pcm_path(path, sizeof(path), session->id, "rx.pcm");
	(void)unlink(path);
	mem_deref(session->call);
}

static struct session *find_session(const char *id)
{
	struct le *le;
	struct session *found = NULL;
	pthread_mutex_lock(&session_lock);
	for (le = sessions.head; le; le = le->next) {
		struct session *session = le->data;
		if (0 == strcmp(session->id, id)) {
			found = mem_ref(session);
			break;
		}
	}
	pthread_mutex_unlock(&session_lock);
	return found;
}

static struct session *find_call_session(struct call *call)
{
	if (!call)
		return NULL;
	return find_session(call_id(call));
}

static void emit_event(const char *type, const struct session *session,
			       const char *extra)
{
	char message[EVENT_BUFFER];
	int len;

	if (!session)
		return;
	len = re_snprintf(message, sizeof(message),
			"{\"version\":1,\"type\":\"%s\",\"call_id\":\"%s\"%s}\n",
			type, session->id, extra ? extra : "");
	if (len < 0)
		return;
	pthread_mutex_lock(&client_lock);
	if (client_fd >= 0 && send(client_fd, message, (size_t)len, MSG_NOSIGNAL) < 0) {
		close(client_fd);
		client_fd = -1;
	}
	pthread_mutex_unlock(&client_lock);
}

static void call_dtmf_handler(struct call *call, char key, void *arg)
{
	struct session *session = arg;
	char extra[64];
	(void)call;
	re_snprintf(extra, sizeof(extra), ",\"digit\":\"%c\",\"phase\":\"end\"", key);
	emit_event("dtmf", session, extra);
}

static void ignore_call_event_handler(struct call *call, enum call_event event,
					     const char *str, void *arg)
{
	(void)call;
	(void)event;
	(void)str;
	(void)arg;
}

static void session_close(struct session *session)
{
	bool was_closed;

	if (!session)
		return;
	pthread_mutex_lock(&session_lock);
	was_closed = session->closed;
	session->closed = true;
	pthread_mutex_unlock(&session_lock);
	if (was_closed)
		return;
	emit_event("call_closed", session, NULL);
	mem_deref(session);
}

static void call_event_handler(struct call *call, enum call_event event,
				       const char *str, void *arg)
{
	struct session *session = arg;
	(void)str;
	if (event == CALL_EVENT_ESTABLISHED)
		emit_event("call_established", session, NULL);
	else if (event == CALL_EVENT_CLOSED)
		session_close(session);
	(void)call;
}

static int new_session(struct call *call, bool outgoing)
{
	struct session *session = mem_zalloc(sizeof(*session), session_destructor);
	char txpath[AUDIO_PATH_MAX];
	char rxpath[AUDIO_PATH_MAX];
	if (!session)
		return ENOMEM;
	session->call = mem_ref(call);
	re_snprintf(session->id, sizeof(session->id), "%s", call_id(call));
	re_snprintf(audio_cur_id, sizeof(audio_cur_id), "%s", session->id);
	(void)mkdir(audio_root, 0700);
	pcm_path(txpath, sizeof(txpath), session->id, "tx.pcm");
	pcm_path(rxpath, sizeof(rxpath), session->id, "rx.pcm");
	(void)unlink(txpath);
	(void)mkfifo(txpath, 0600);
	(void)unlink(rxpath);
	(void)mkfifo(rxpath, 0600);
	if (call_audio(call))
		(void)audio_set_devicename(call_audio(call), session->id, session->id);
	call_set_handlers(call, call_event_handler, call_dtmf_handler, session);
	pthread_mutex_lock(&session_lock);
	list_append(&sessions, &session->le, session);
	pthread_mutex_unlock(&session_lock);
	char extra[256];
	re_snprintf(extra, sizeof(extra), ",\"from\":\"%s\",\"tx_path\":\"%s\",\"rx_path\":\"%s\"", call_peeruri(call), txpath, rxpath);
	/* Outbound (alert) calls are dialed by Go, so admission is already given.
	 * Inbound calls are admitted by Go; do not answer an unwhitelisted caller. */
	emit_event(outgoing ? "call_outgoing" : "call_incoming", session, extra);
	return 0;
}

static void dial_uri(const char *uri)
{
	struct ua *ua = NULL;
	struct le *le;
	struct call *call = NULL;
	int err;

	if (!str_isset(uri))
		return;
	le = list_head(uag_list());
	while (le) {
		if (le->data) {
			ua = le->data;
			break;
		}
		le = le->next;
	}
	if (!ua) {
		warning("voxmail: dial: no account configured\n");
		return;
	}
	err = ua_connect(ua, &call, NULL, uri, VIDMODE_OFF);
	if (err)
		warning("voxmail: dial failed (%m), uri %s\n", err, uri);
}

static void dial_pipe_handler(int flags, void *arg)
{
	char uri[256];
	ssize_t n;
	(void)flags;
	(void)arg;
	while ((n = read(dial_pipe[0], uri, sizeof(uri) - 1)) > 0) {
		uri[n] = '\0';
		dial_uri(uri);
	}
}

static int dial_open(void)
{
	if (pipe(dial_pipe) < 0)
		return errno;
	(void)fcntl(dial_pipe[0], F_SETFL, O_NONBLOCK);
	if (fd_listen(&dial_fhs, (re_sock_t)dial_pipe[0], FD_READ,
		     dial_pipe_handler, NULL) < 0) {
		close(dial_pipe[0]);
		close(dial_pipe[1]);
		dial_pipe[0] = dial_pipe[1] = -1;
		return errno;
	}
	return 0;
}

static void compact_json(char *s)
{
	size_t i = 0, j = 0;
	while (s[i]) {
		if (s[i] != ' ' && s[i] != '\t' && s[i] != '\r' &&
		    s[i] != '\n')
			s[j++] = s[i];
		i++;
	}
	s[j] = '\0';
}

static void dial_request(const char *uri)
{
	if (!str_isset(uri))
		return;
	if (dial_pipe[1] < 0)
		return;
	if (write(dial_pipe[1], uri, strlen(uri)) < 0)
		warning("voxmail: dial queue write failed: %m\n");
}

static void baresip_event_handler(enum bevent_ev event, struct bevent *bevent,
					 void *arg)
{
	(void)arg;
	switch (event) {
	case BEVENT_CALL_INCOMING:
		(void)new_session(bevent_get_call(bevent), false);
		break;
	case BEVENT_CALL_OUTGOING:
		(void)new_session(bevent_get_call(bevent), true);
		break;
	case BEVENT_CALL_ESTABLISHED: {
		struct session *session =
			find_call_session(bevent_get_call(bevent));
		if (session) {
			emit_event("call_established", session, NULL);
			mem_deref(session);
		}
		break;
	}
	default:
		break;
	}
}

static void *socket_worker(void *arg)
{
	(void)arg;
	while (running) {
		int fd = accept(server_fd, NULL, NULL);
		if (fd < 0) {
			if (errno == EINTR)
				continue;
			if (!running)
				break;
			continue;
		}
		pthread_mutex_lock(&client_lock);
		if (client_fd >= 0)
			close(client_fd);
		client_fd = fd;
		pthread_mutex_unlock(&client_lock);
		char buffer[EVENT_BUFFER];
		while (running) {
			ssize_t len = recv(fd, buffer, sizeof(buffer)-1, 0);
			if (len <= 0) break;
			buffer[len] = '\0';
			compact_json(buffer);
			if (strstr(buffer, "\"type\":\"dial\"")) {
				char *uri = strstr(buffer, "\"to\":\"");
				if (uri) {
					uri += strlen("\"to\":\"");
					char *end = strchr(uri, '\"');
					if (end) *end = '\0';
					dial_request(uri);
				}
				continue;
			}
			char *id = strstr(buffer, "\"call_id\":\"");
			if (!id) continue;
			id += strlen("\"call_id\":\"");
			char *end = strchr(id, '\"'); if (end) *end = '\0';
			struct session *session = find_session(id); if (!session) continue;
			if (strstr(buffer, "\"type\":\"hangup\"")) {
				call_set_handlers(session->call,
						  ignore_call_event_handler,
						  NULL, session);
				call_hangup(session->call, 603,
					    "Caller not authorized");
				session_close(session);
				continue;
			}
			else if (strstr(buffer, "\"type\":\"answer\"")) {
				(void)call_answer(session->call, 200, VIDMODE_OFF);
			}
			mem_deref(session);
		}
		pthread_mutex_lock(&client_lock);
		if (client_fd == fd) client_fd = -1;
		pthread_mutex_unlock(&client_lock);
		close(fd);
	}
	return NULL;
}

static int open_socket(void)
{
	struct sockaddr_un address;
	const char *path = getenv("VOXMAIL_CONTROL_SOCKET");
	if (!path || strlen(path) >= sizeof(socket_path))
		return EINVAL;
	strcpy(socket_path, path);
	server_fd = socket(AF_UNIX, SOCK_STREAM, 0);
	if (server_fd < 0)
		return errno;
	unlink(socket_path);
	memset(&address, 0, sizeof(address));
	address.sun_family = AF_UNIX;
	strcpy(address.sun_path, socket_path);
	if (bind(server_fd, (struct sockaddr *)&address, sizeof(address)) < 0 ||
		listen(server_fd, 1) < 0)
		return errno;
	return 0;
}

static int module_init(void)
{
	int err;
	const char *root = getenv("VOXMAIL_AUDIO_DIR");
	if (root && strlen(root) < sizeof(audio_root))
		re_snprintf(audio_root, sizeof(audio_root), "%s", root);
	(void)signal(SIGPIPE, SIG_IGN);
	list_init(&sessions);
	(void)mkdir(audio_root, 0700);
	err = ausrc_register(&pcm_ausrc, baresip_ausrcl(), "voxmail", pcm_source_alloc);
	if (err)
		return err;
	err = auplay_register(&pcm_auplay, baresip_auplayl(), "voxmail", pcm_player_alloc);
	if (err)
		return err;
	err = open_socket();
	if (err)
		return err;
	err = dial_open();
	if (err)
		return err;
	err = bevent_register(baresip_event_handler, 0);
	if (err)
		return err;
	running = 1;
	if (pthread_create(&socket_thread, NULL, socket_worker, NULL))
		return errno;
	return 0;
}

static int module_close(void)
{
	running = 0;
	pcm_ausrc = mem_deref(pcm_ausrc);
	pcm_auplay = mem_deref(pcm_auplay);
	bevent_unregister(baresip_event_handler);
	dial_fhs = mem_deref(dial_fhs);
	if (dial_pipe[0] >= 0)
		close(dial_pipe[0]);
	if (dial_pipe[1] >= 0)
		close(dial_pipe[1]);
	dial_pipe[0] = dial_pipe[1] = -1;
	if (server_fd >= 0) {
		shutdown(server_fd, SHUT_RDWR);
		close(server_fd);
		server_fd = -1;
	}
	pthread_mutex_lock(&client_lock);
	if (client_fd >= 0)
		shutdown(client_fd, SHUT_RDWR);
	pthread_mutex_unlock(&client_lock);
	pthread_join(socket_thread, NULL);
	pthread_mutex_lock(&client_lock);
	if (client_fd >= 0) {
		close(client_fd);
		client_fd = -1;
	}
	pthread_mutex_unlock(&client_lock);
	unlink(socket_path);
	list_flush(&sessions);
	return 0;
}

const struct mod_export DECL_EXPORTS(voxmail) = {
	"voxmail", "application", module_init, module_close
};
