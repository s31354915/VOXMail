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
#include <poll.h>
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
#include "command_queue.h"
#include "call_dispatch.h"
#include "client_owner.h"
#include "command_wakeup.h"
#include "frame_parser.h"
#include "json_output.h"
#include "nonblocking_pipe.h"
#include "pcm_writer.h"
#include "pthread_start.h"

enum { SOCKET_PATH_MAX = 108, EVENT_BUFFER = 2048, AUDIO_PATH_MAX = 256,
	DIAL_ID_MAX = 128, DIAL_URI_MAX = 256, COMMAND_QUEUE_MAX = 256,
	EVENT_QUEUE_MAX = 256, COMMANDS_PER_TURN = 32,
	EVENT_SEND_TIMEOUT_MS = 1000, ACCEPT_POLL_TIMEOUT_MS = 100,
	VOXMAIL_SAMPLE_RATE = 8000, VOXMAIL_CHANNELS = 1,
	VOXMAIL_MAX_CAPTURE_BYTES = 64 * 1024 * 1024 };

struct session {
	struct le le;
	struct call *call;
	char id[128];
	char request_id[DIAL_ID_MAX];
	bool listed;
	bool closed;
	bool ringing;
	bool established;
};

struct dial_item {
	struct le le;
	char request_id[DIAL_ID_MAX];
	char uri[DIAL_URI_MAX];
};

static struct list sessions;
static pthread_t socket_thread;
static pthread_t event_thread;
static bool socket_thread_started;
static bool event_thread_started;
static bool bevent_started;
static RE_ATOMIC bool running;
static int server_fd = -1;
/* The socket worker owns close(); other threads only use shutdown() to wake
 * it and invalidate the published generation. */
static struct voxmail_client_owner client_owner =
	VOXMAIL_CLIENT_OWNER_INITIALIZER;
static pthread_mutex_t session_lock = PTHREAD_MUTEX_INITIALIZER;
static char socket_path[SOCKET_PATH_MAX];
static char audio_root[AUDIO_PATH_MAX] = "/data/run/voxmail";

struct pcm_source {
	uint32_t ptime;
	size_t sampc;
	int fd;
	RE_ATOMIC bool run;
	thrd_t thread;
	bool thread_started;
	struct ausrc_prm prm;
	ausrc_read_h *rh;
	void *arg;
};

struct pcm_player {
	int fd;
	char device[AUDIO_PATH_MAX];
	struct voxmail_pcm_writer writer;
	struct auplay_prm prm;
	auplay_write_h *wh;
	void *arg;
	size_t sampc;
	RE_ATOMIC bool run;
	thrd_t thread;
	bool thread_started;
};

static struct ausrc *pcm_ausrc;
static struct auplay *pcm_auplay;

static int dial_pipe[2] = { -1, -1 };
static struct re_fhs *dial_fhs;
static struct list dial_queue;
static pthread_mutex_t dial_lock = PTHREAD_MUTEX_INITIALIZER;
/* Baresip 4.11 emits CALL_EVENT_OUTGOING synchronously inside the exact
 * ua_connect() invocation. Bind that callback to the active invocation, not
 * merely to whichever request happens to be first in the queue. */
static struct dial_item *active_dial_item;
static struct voxmail_command_queue command_queue;
struct event_item {
	struct le le;
	size_t len;
	char message[EVENT_BUFFER];
};
static struct list event_queue;
static size_t event_count;
static pthread_mutex_t event_lock = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t event_cond = PTHREAD_COND_INITIALIZER;

static int pcm_path(char *path, size_t sz, const char *device, const char *suffix)
{
	const char *id = str_isset(device) ? device : "default";
	int len;
	if (!path || sz == 0 || !suffix)
		return EINVAL;
	while (*id == ',') ++id;
	len = re_snprintf(path, sz, "%s/%s.%s", audio_root, id, suffix);
	return len < 0 || (size_t)len >= sz ? EOVERFLOW : 0;
}

static void pcm_source_destructor(void *arg)
{
	struct pcm_source *st = arg;
	if (st->thread_started) {
		re_atomic_rlx_set(&st->run, false);
		thrd_join(st->thread, NULL);
		st->thread_started = false;
	}
	if (st->fd >= 0)
		close(st->fd);
}

static int pcm_source_thread(void *arg)
{
	struct pcm_source *st = arg;
	size_t bytes = st->sampc * aufmt_sample_size(st->prm.fmt);
	void *samples = mem_zalloc(bytes, NULL);
	void *pending = mem_zalloc(bytes, NULL);
	size_t pending_len = 0;
	if (!samples || !pending) {
		mem_deref(samples);
		mem_deref(pending);
		return ENOMEM;
	}
	while (re_atomic_rlx(&st->run)) {
		struct auframe af;
		ssize_t got = 0;
		memset(samples, 0, bytes);
		if (st->fd >= 0 && pending_len < bytes)
			got = read(st->fd, (char *)pending + pending_len,
			           bytes - pending_len);
		if (got < 0 && errno != EAGAIN && errno != EINTR)
			break;
		if (got > 0)
			pending_len += (size_t)got;
		if (pending_len == bytes) {
			memcpy(samples, pending, bytes);
			pending_len = 0;
		}
		/* A FIFO opened read-only/nonblocking reports EOF while no writer is
		 * attached.  Keep the source alive and emit silence until Go opens the
		 * corresponding TX pipe.  If a writer supplied only part of one PCM
		 * frame, keep those bytes private until the rest arrives; never expose a
		 * half sample or a mixed frame to baresip. */
		auframe_init(&af, st->prm.fmt, samples, st->sampc,
		             st->prm.srate, st->prm.ch);
		st->rh(&af, st->arg);
		sys_msleep(st->ptime ? st->ptime : 20);
	}
	mem_deref(samples);
	mem_deref(pending);
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
	if (!stp || !prm || !rh || prm->fmt != AUFMT_S16LE ||
	    prm->srate != VOXMAIL_SAMPLE_RATE || prm->ch != VOXMAIL_CHANNELS)
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
	err = pcm_path(path, sizeof(path), str_isset(device) ? device : "default",
	              "tx.pcm");
	if (err) {
		mem_deref(st);
		return err;
	}
	st->fd = open(path, O_RDONLY | O_NONBLOCK);
	if (st->fd < 0) {
		err = errno;
		mem_deref(st);
		return err;
	}
	re_atomic_rlx_set(&st->run, true);
	err = thread_create_name(&st->thread, "voxmail_pcm_in", pcm_source_thread, st);
	if (err) {
		re_atomic_rlx_set(&st->run, false);
		mem_deref(st);
		return err;
	}
	st->thread_started = true;
	*stp = (struct ausrc_st *)st;
	return 0;
}

static void pcm_player_destructor(void *arg)
{
	struct pcm_player *st = arg;
	if (st->thread_started) {
		re_atomic_rlx_set(&st->run, false);
		thrd_join(st->thread, NULL);
		st->thread_started = false;
	}
	if (st->fd >= 0)
		close(st->fd);
}

static int pcm_player_thread(void *arg)
{
	struct pcm_player *st = arg;
	size_t bytes = st->sampc * aufmt_sample_size(st->prm.fmt);
	char path[AUDIO_PATH_MAX];
	void *samples = mem_zalloc(bytes, NULL);
	if (!samples)
		return ENOMEM;
	if (pcm_path(path, sizeof(path), st->device, "rx.pcm")) {
		warning("voxmail: PCM output path is too long; stopping media writer\n");
		re_atomic_rlx_set(&st->run, false);
		mem_deref(samples);
		return EOVERFLOW;
	}
	while (re_atomic_rlx(&st->run)) {
		struct auframe af;
		memset(samples, 0, bytes);
		auframe_init(&af, st->prm.fmt, samples, st->sampc,
		             st->prm.srate, st->prm.ch);
		st->wh(&af, st->arg);
		if (st->fd < 0) {
			st->fd = open(path, O_WRONLY | O_NONBLOCK | O_APPEND, 0600);
		}
		if (st->fd >= 0) {
			int write_err = voxmail_pcm_writer_write(&st->writer, st->fd,
			                                        samples, bytes);
			if (write_err == EPIPE || write_err == ENXIO) {
				close(st->fd);
				st->fd = -1;
			}
			else if (write_err && write_err != EAGAIN && write_err != EINTR) {
				warning("voxmail: bounded PCM capture stopped (%m)\n", write_err);
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
	int err;
	(void)ap;
	if (!stp || !prm || !wh || prm->fmt != AUFMT_S16LE ||
	    prm->srate != VOXMAIL_SAMPLE_RATE || prm->ch != VOXMAIL_CHANNELS)
		return EINVAL;
	st = mem_zalloc(sizeof(*st), pcm_player_destructor);
	if (!st)
		return ENOMEM;
	st->fd = -1;
	voxmail_pcm_writer_init(&st->writer, VOXMAIL_MAX_CAPTURE_BYTES);
	st->prm = *prm;
	st->device[0] = '\0';
	{
		int copied = re_snprintf(st->device, sizeof(st->device), "%s",
		                         str_isset(device) ? device : "default");
		if (copied < 0 || (size_t)copied >= sizeof(st->device)) {
			mem_deref(st);
			return EOVERFLOW;
		}
	}
	st->wh = wh;
	st->arg = arg;
	st->sampc = prm->srate * prm->ch * (prm->ptime ? prm->ptime : 20) / 1000;
	err = pcm_path(path, sizeof(path), st->device, "rx.pcm");
	if (err) {
		mem_deref(st);
		return err;
	}
	st->fd = open(path, O_WRONLY | O_NONBLOCK | O_CREAT | O_APPEND, 0600);
	if (st->fd < 0 && errno == ENOENT) {
		if (mkfifo(path, 0600) < 0 && errno != EEXIST) {
			err = errno;
			mem_deref(st);
			return err;
		}
		st->fd = open(path, O_WRONLY | O_NONBLOCK | O_APPEND, 0600);
	}
	if (st->fd < 0 && errno != ENXIO) {
		err = errno;
		mem_deref(st);
		return err;
	}
	re_atomic_rlx_set(&st->run, true);
	err = thread_create_name(&st->thread, "voxmail_pcm_out", pcm_player_thread, st);
	if (err) {
		re_atomic_rlx_set(&st->run, false);
		mem_deref(st);
		return err;
	}
	st->thread_started = true;
	*stp = (struct auplay_st *)st;
	return 0;
}

static void session_destructor(void *arg)
{
	struct session *session = arg;
	char path[AUDIO_PATH_MAX];
	if (session->listed) {
		pthread_mutex_lock(&session_lock);
		list_unlink(&session->le);
		session->listed = false;
		pthread_mutex_unlock(&session_lock);
	}
	if (!pcm_path(path, sizeof(path), session->id, "tx.pcm"))
		(void)unlink(path);
	if (!pcm_path(path, sizeof(path), session->id, "rx.pcm"))
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

/* The bridge is newline-delimited JSON. Use libre's bounded JSON decoder so
 * whitespace, escaped strings, and field ordering follow the JSON contract
 * rather than a collection of compact-spelling assumptions. */
static bool copy_json_string(const char *value, char *out, size_t sz)
{
	if (!value || !out || sz == 0 || strlen(value) >= sz)
		return false;
	re_snprintf(out, sz, "%s", value);
	return true;
}

static int send_all(int fd, const char *message, size_t len)
{
	size_t sent = 0;
	while (sent < len) {
		ssize_t n = send(fd, message + sent, len - sent,
		                 MSG_NOSIGNAL | MSG_DONTWAIT);
		if (n < 0 && errno == EINTR)
			continue;
		if (n < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) {
			struct pollfd pfd = {fd, POLLOUT, 0};
			int nready;
			do {
				nready = poll(&pfd, 1, EVENT_SEND_TIMEOUT_MS);
			} while (nready < 0 && errno == EINTR);
			if (nready == 0)
				return ETIMEDOUT;
			if (nready < 0)
				return errno;
			if (pfd.revents & (POLLERR | POLLHUP | POLLNVAL))
				return EPIPE;
			continue;
		}
		if (n <= 0)
			return errno ? errno : EPIPE;
		sent += (size_t)n;
	}
	return 0;
}

static void *event_worker(void *arg)
{
	(void)arg;
	for (;;) {
		struct event_item *item = NULL;
		int err = 0;

		pthread_mutex_lock(&event_lock);
		while (event_count == 0 && re_atomic_rlx(&running))
			pthread_cond_wait(&event_cond, &event_lock);
		if (event_count > 0) {
			item = event_queue.head->data;
			list_unlink(&item->le);
			--event_count;
		}
		else if (!re_atomic_rlx(&running)) {
			pthread_mutex_unlock(&event_lock);
			break;
		}
		pthread_mutex_unlock(&event_lock);

		if (!item)
			continue;
		int source_fd = -1;
		int writer_fd = -1;
		uint64_t generation = 0;
		int snapshot_err = voxmail_client_owner_snapshot_dup(&client_owner,
		                                                     &source_fd,
		                                                     &generation,
		                                                     &writer_fd);
		err = snapshot_err;
		if (!err)
			err = send_all(writer_fd, item->message, item->len);
		if (writer_fd >= 0)
			close(writer_fd);
		if (snapshot_err == ENOTCONN) {
			/* Registration can complete before the Go bridge has accepted its
			 * first Unix-socket connection. Keep the whole frame at the head of
			 * the bounded queue until a consumer is available; dropping it makes
			 * readiness depend on an uncontrolled startup race. */
			pthread_mutex_lock(&event_lock);
			if (re_atomic_rlx(&running)) {
				list_prepend(&event_queue, &item->le, item);
				++event_count;
				item = NULL;
			}
			pthread_mutex_unlock(&event_lock);
			if (!item)
				sys_msleep(10);
			else
				mem_deref(item);
			continue;
		}
		if (err) {
			warning("voxmail: control event delivery failed (%m); reconnect required\n",
			        err);
			(void)voxmail_client_owner_invalidate_if_current(&client_owner,
			                                                source_fd, generation);
		}
		mem_deref(item);
	}
	return NULL;
}

static void enqueue_event(const char *message, size_t len)
{
	struct event_item *item;
	bool overflow = false;

	if (!message || len == 0 || len >= EVENT_BUFFER ||
	    !re_atomic_rlx(&running))
		return;
	item = mem_zalloc(sizeof(*item), NULL);
	if (!item)
		return;
	memcpy(item->message, message, len);
	item->message[len] = '\0';
	item->len = len;
	pthread_mutex_lock(&event_lock);
	if (!re_atomic_rlx(&running)) {
		pthread_mutex_unlock(&event_lock);
		mem_deref(item);
		return;
	}
	if (event_count >= EVENT_QUEUE_MAX)
		overflow = true;
	if (overflow) {
		pthread_mutex_unlock(&event_lock);
		warning("voxmail: control event queue full; disconnecting bridge\n");
		voxmail_client_owner_request_shutdown(&client_owner);
		mem_deref(item);
		return;
	}
	list_append(&event_queue, &item->le, item);
	++event_count;
	pthread_cond_signal(&event_cond);
	pthread_mutex_unlock(&event_lock);
}

static void emit_event(const char *type, const struct session *session,
			       const char *extra)
{
	char message[EVENT_BUFFER];
	char escaped_id[sizeof(session->id) * 6 + 1];
	int len;

	if (!session)
		return;
	if (!voxmail_json_escape(escaped_id, sizeof(escaped_id), session->id)) {
		warning("voxmail: event call_id cannot be encoded without truncation\n");
		return;
	}
	len = re_snprintf(message, sizeof(message),
			"{\"version\":2,\"type\":\"%s\",\"call_id\":\"%s\"%s}\n",
			type, escaped_id, extra ? extra : "");
	if (len <= 0 || (size_t)len >= sizeof(message)) {
		warning("voxmail: event too large; refusing truncated output\n");
		return;
	}
	enqueue_event(message, (size_t)len);
}

static void emit_registration_event(enum bevent_ev event,
					    const struct bevent *bevent)
{
	const char *phase;
	const char *reason = bevent_get_text(bevent);
	char escaped_reason[256 * 6 + 1];
	char message[EVENT_BUFFER];
	int len;

	switch (event) {
	case BEVENT_REGISTERING:
		phase = "registering";
		break;
	case BEVENT_REGISTER_OK:
		phase = "registered";
		break;
	case BEVENT_REGISTER_FAIL:
		phase = "failed";
		break;
	case BEVENT_UNREGISTERING:
		phase = "unregistered";
		break;
	default:
		return;
	}
	if (!reason)
		reason = "";
	if (!voxmail_json_escape(escaped_reason, sizeof(escaped_reason), reason))
		return;
	len = re_snprintf(message, sizeof(message),
			"{\"version\":2,\"type\":\"registration\","
			"\"phase\":\"%s\",\"reason\":\"%s\"}\n",
			phase, escaped_reason);
	if (len <= 0 || (size_t)len >= sizeof(message)) {
		warning("voxmail: registration event too large; refusing truncated output\n");
		return;
	}
	enqueue_event(message, (size_t)len);
}

static void emit_command_error(const char *request_id, const char *call_id,
				       int code, const char *reason)
{
	char message[EVENT_BUFFER];
	char escaped_request[DIAL_ID_MAX * 6 + 1];
	char escaped_call[sizeof(((struct session *)0)->id) * 6 + 1];
	char escaped_reason[256 * 6 + 1];
	char fields[EVENT_BUFFER];
	size_t fields_len = 0;
	int len;

	if (!voxmail_json_escape(escaped_request, sizeof(escaped_request),
	                         request_id) ||
	    !voxmail_json_escape(escaped_call, sizeof(escaped_call), call_id) ||
	    !voxmail_json_escape(escaped_reason, sizeof(escaped_reason), reason)) {
		warning("voxmail: command error cannot be encoded without truncation\n");
		return;
	}
	fields[0] = '\0';
	if (str_isset(request_id))
		if (!voxmail_json_append(fields, sizeof(fields), &fields_len,
		                         ",\"request_id\":\"%s\"", escaped_request))
			return;
	if (str_isset(call_id))
		if (!voxmail_json_append(fields, sizeof(fields), &fields_len,
		                         ",\"call_id\":\"%s\"", escaped_call))
			return;
	len = re_snprintf(message, sizeof(message),
			"{\"version\":2,\"type\":\"command_error\"%s,\"code\":%d,\"reason\":\"%s\"}\n",
			fields, code, escaped_reason);
	if (len <= 0 || (size_t)len >= sizeof(message)) {
		warning("voxmail: command error too large; refusing truncated output\n");
		return;
	}
	enqueue_event(message, (size_t)len);
}

static void call_dtmf_handler(struct call *call, char key, void *arg)
{
	struct session *session = arg;
	char extra[64];
	char digit[2] = {(char)(unsigned char)key, '\0'};
	size_t used = 0;
	(void)call;
	{
		char escaped_digit[sizeof(digit) * 6 + 1];
		if (!voxmail_json_escape(escaped_digit, sizeof(escaped_digit), digit) ||
		    !voxmail_json_append(extra, sizeof(extra), &used,
		                         ",\"digit\":\"%s\",\"phase\":\"end\"",
		                         escaped_digit))
			return;
	}
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
	bool emit_established = false;
	bool emit_ringing = false;
	(void)str;
	if (event == CALL_EVENT_RINGING) {
		pthread_mutex_lock(&session_lock);
		if (!session->ringing && !session->closed) {
			session->ringing = true;
			emit_ringing = true;
		}
		pthread_mutex_unlock(&session_lock);
	}
	else if (event == CALL_EVENT_ESTABLISHED) {
		/* Some baresip paths can report establishment through more than one
		 * callback. The Go side must see one transition per call. */
		pthread_mutex_lock(&session_lock);
		if (!session->established && !session->closed) {
			session->established = true;
			emit_established = true;
		}
		pthread_mutex_unlock(&session_lock);
	}
	if (emit_ringing)
		emit_event("call_ringing", session, NULL);
	else if (emit_established)
		emit_event("call_established", session, NULL);
	else if (event == CALL_EVENT_CLOSED)
		session_close(session);
	(void)call;
}

static struct dial_item *take_dial_request(void)
{
	struct dial_item *item;

	pthread_mutex_lock(&dial_lock);
	item = active_dial_item;
	if (item && item->le.list) {
		active_dial_item = NULL;
		/* The queue owns one reference. Transfer a separate reference to the
		 * outgoing-event handler before releasing that queue ownership. The
		 * caller of this function must release the returned reference. */
		list_unlink(&item->le);
		mem_ref(item);
		mem_deref(item);
	}
	else {
		item = NULL;
		active_dial_item = NULL;
	}
	pthread_mutex_unlock(&dial_lock);
	return item;
}

static int queue_command(enum voxmail_command_type type, const char *request_id,
				 const char *uri, const char *call_id, int code,
				 const char *reason)
{
	struct voxmail_command item = {0};
	int err;

	item.type = type;
	if (request_id)
		re_snprintf(item.request_id, sizeof(item.request_id), "%s",
			     request_id);
	if (uri)
		re_snprintf(item.uri, sizeof(item.uri), "%s", uri);
	if (call_id)
		re_snprintf(item.call_id, sizeof(item.call_id), "%s", call_id);
	item.code = code;
	if (reason)
		re_snprintf(item.reason, sizeof(item.reason), "%s", reason);
	if (dial_pipe[1] < 0) {
		return EPIPE;
	}
	err = voxmail_command_queue_push(&command_queue, &item);
	if (err)
		return err;
	err = voxmail_command_wakeup(dial_pipe[1]);
	if (err)
		voxmail_command_queue_stop(&command_queue);
	return err;
}

static int new_session(struct call *call, bool outgoing)
{
	struct dial_item *dial_item = NULL;
	struct session *session;
	char txpath[AUDIO_PATH_MAX];
	char rxpath[AUDIO_PATH_MAX];
	char escaped_from[AUDIO_PATH_MAX * 6 + 1];
	char escaped_tx[AUDIO_PATH_MAX * 6 + 1];
	char escaped_rx[AUDIO_PATH_MAX * 6 + 1];
	char escaped_request[DIAL_ID_MAX * 6 + 1];
	char request_id[DIAL_ID_MAX] = "";
	char extra[EVENT_BUFFER];
	size_t extra_len = 0;
	int encoding_err;
	int path_err;

	/* Baresip emits CALL_EVENT_OUTGOING synchronously from call_connect(),
	 * before ua_connect() returns. Consume the matching queue ownership at
	 * callback entry so every later failure leaves no stale request behind. */
	if (outgoing)
		dial_item = take_dial_request();
	session = mem_zalloc(sizeof(*session), session_destructor);
	if (!session) {
		mem_deref(dial_item);
		return ENOMEM;
	}
	if (!call) {
		mem_deref(session);
		mem_deref(dial_item);
		return EINVAL;
	}
	if (dial_item) {
		if (!copy_json_string(dial_item->request_id, request_id,
		                      sizeof(request_id))) {
			mem_deref(session);
			mem_deref(dial_item);
			return EOVERFLOW;
		}
	}
	session->call = mem_ref(call);
	if (!copy_json_string(call_id(call), session->id, sizeof(session->id))) {
		mem_deref(session);
		mem_deref(dial_item);
		return EOVERFLOW;
	}
	if (!copy_json_string(request_id, session->request_id,
	                      sizeof(session->request_id))) {
		mem_deref(session);
		mem_deref(dial_item);
		return EOVERFLOW;
	}
	if (mkdir(audio_root, 0700) < 0 && errno != EEXIST) {
		int err = errno;
		mem_deref(session);
		mem_deref(dial_item);
		return err;
	}
	path_err = pcm_path(txpath, sizeof(txpath), session->id, "tx.pcm");
	if (path_err) {
		mem_deref(session);
		mem_deref(dial_item);
		return path_err;
	}
	path_err = pcm_path(rxpath, sizeof(rxpath), session->id, "rx.pcm");
	if (path_err) {
		mem_deref(session);
		mem_deref(dial_item);
		return path_err;
	}
	(void)unlink(txpath);
	if (mkfifo(txpath, 0600) < 0) {
		int err = errno;
		mem_deref(session);
		mem_deref(dial_item);
		return err;
	}
	(void)unlink(rxpath);
	/* Go reads captured audio with regular-file offsets. A FIFO cannot be
	 * seeked or sized, so keep this private network-to-Go stream as an
	 * append-only PCM file. */
	int rxfd = open(rxpath, O_CREAT | O_WRONLY | O_TRUNC, 0600);
	if (rxfd < 0) {
		int err = errno;
		mem_deref(session);
		mem_deref(dial_item);
		return err;
	}
	close(rxfd);
	if (!voxmail_json_escape(escaped_from, sizeof(escaped_from),
	                         call_peeruri(call)) ||
	    !voxmail_json_escape(escaped_tx, sizeof(escaped_tx), txpath) ||
	    !voxmail_json_escape(escaped_rx, sizeof(escaped_rx), rxpath) ||
	    (outgoing && !voxmail_json_escape(escaped_request,
	                                      sizeof(escaped_request), request_id))) {
		encoding_err = EOVERFLOW;
		(void)unlink(txpath);
		(void)unlink(rxpath);
		mem_deref(session);
		mem_deref(dial_item);
		return encoding_err;
	}
	extra[0] = '\0';
	if (outgoing) {
		if (!voxmail_json_append(extra, sizeof(extra), &extra_len,
		                         ",\"from\":\"%s\",\"tx_path\":\"%s\",\"rx_path\":\"%s\",\"request_id\":\"%s\"",
		                         escaped_from, escaped_tx, escaped_rx,
		                         escaped_request)) {
			encoding_err = EOVERFLOW;
			(void)unlink(txpath);
			(void)unlink(rxpath);
			mem_deref(session);
			mem_deref(dial_item);
			return encoding_err;
		}
	}
	else if (!voxmail_json_append(extra, sizeof(extra), &extra_len,
	                              ",\"from\":\"%s\",\"tx_path\":\"%s\",\"rx_path\":\"%s\"",
	                              escaped_from, escaped_tx, escaped_rx)) {
		encoding_err = EOVERFLOW;
			(void)unlink(txpath);
			(void)unlink(rxpath);
			mem_deref(session);
			mem_deref(dial_item);
			return encoding_err;
	}
	if (call_audio(call))
		(void)audio_set_devicename(call_audio(call), session->id, session->id);
	call_set_handlers(call, call_event_handler, call_dtmf_handler, session);
	pthread_mutex_lock(&session_lock);
	list_append(&sessions, &session->le, session);
	session->listed = true;
	pthread_mutex_unlock(&session_lock);
	/* Outbound (alert) calls are dialed by Go, so admission is already given.
	 * Inbound calls are admitted by Go; do not answer an unwhitelisted caller. */
	emit_event(outgoing ? "call_outgoing" : "call_incoming", session, extra);
	mem_deref(dial_item);
	return 0;
}

static void dial_uri(const char *request_id, const char *uri)
{
	struct ua *ua = NULL;
	struct le *le;
	struct call *call = NULL;
	struct dial_item *item;
	int err;

	if (!str_isset(uri) || !str_isset(request_id))
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
		emit_command_error(request_id, NULL, 503, "no SIP account configured");
		return;
	}
	item = mem_zalloc(sizeof(*item), NULL);
	if (!item) {
		emit_command_error(request_id, NULL, 500, "dial allocation failed");
		return;
	}
	re_snprintf(item->request_id, sizeof(item->request_id), "%s", request_id);
	re_snprintf(item->uri, sizeof(item->uri), "%s", uri);
	pthread_mutex_lock(&dial_lock);
	/* The allocation reference is local to dial_uri(); the list owns this
	 * additional reference until the outgoing callback or an error path takes
	 * it. This keeps item memory valid across synchronous callback execution. */
	mem_ref(item);
	list_append(&dial_queue, &item->le, item);
	active_dial_item = item;
	pthread_mutex_unlock(&dial_lock);
	err = ua_connect(ua, &call, NULL, uri, VIDMODE_OFF);
	if (err) {
		bool queued = false;
		pthread_mutex_lock(&dial_lock);
		if (active_dial_item == item)
			active_dial_item = NULL;
		if (item->le.list) {
			list_unlink(&item->le);
			queued = true;
		}
		pthread_mutex_unlock(&dial_lock);
		if (queued)
			mem_deref(item); /* release the queue reference */
		warning("voxmail: dial failed (%m), uri %s\n", err, uri);
		emit_command_error(request_id, NULL, 503, "dial request failed");
	}
	else {
		pthread_mutex_lock(&dial_lock);
		if (active_dial_item == item)
			active_dial_item = NULL;
		pthread_mutex_unlock(&dial_lock);
	}
	mem_deref(item); /* release dial_uri()'s local reference */
}

static void *dispatch_find(void *arg, const char *call_id)
{
	(void)arg;
	return find_session(call_id);
}

static int dispatch_answer(void *arg, void *call_arg)
{
	struct session *session = call_arg;
	(void)arg;
	return call_answer(session->call, 200, VIDMODE_OFF);
}

static int dispatch_hangup(void *arg, void *call_arg, int code,
				   const char *reason)
{
	struct session *session = call_arg;
	(void)arg;
	call_set_handlers(session->call, ignore_call_event_handler, NULL, session);
	call_hangup(session->call, (uint16_t)code, reason);
	return 0;
}

static void dispatch_close(void *arg, void *call_arg)
{
	(void)arg;
	session_close(call_arg);
}

static void dispatch_release(void *arg, void *call_arg)
{
	(void)arg;
	mem_deref(call_arg);
}

static const struct voxmail_call_ops dispatch_ops = {
	dispatch_find,
	dispatch_answer,
	dispatch_hangup,
	dispatch_close,
	dispatch_release,
};

static void execute_command(struct voxmail_command *item)
{
	int err;

	if (!item)
		return;
	if (item->type == VOXMAIL_COMMAND_DIAL) {
		dial_uri(item->request_id, item->uri);
		return;
	}

	err = voxmail_dispatch_call(item, &dispatch_ops, NULL);
	if (err == VOXMAIL_DISPATCH_STALE) {
		emit_command_error(item->request_id, item->call_id, 481,
				   "call no longer exists");
		return;
	}
	if (err)
		emit_command_error(item->request_id, item->call_id, 500,
				   "call command failed");
}

static void execute_command_callback(const struct voxmail_command *item,
					     void *arg)
{
	(void)arg;
	execute_command((struct voxmail_command *)item);
}

static void dial_pipe_handler(int flags, void *arg)
{
	char wake[256];
	ssize_t n;
	size_t processed = 0;
	bool pending = false;
	(void)flags;
	(void)arg;
	while ((n = read(dial_pipe[0], wake, sizeof(wake))) > 0)
		;
	processed = voxmail_command_queue_dispatch(&command_queue,
						  COMMANDS_PER_TURN,
						  execute_command_callback, NULL);
	if (processed == COMMANDS_PER_TURN) {
		pending = voxmail_command_queue_pending(&command_queue);
		if (pending && dial_pipe[1] >= 0) {
			int wake_err = voxmail_command_wakeup(dial_pipe[1]);
			if (wake_err) {
				warning("voxmail: failed to reschedule bounded command batch (%m)\n",
				        wake_err);
				voxmail_command_queue_stop(&command_queue);
			}
		}
	}
}

static int dial_open(void)
{
	int err;
	err = voxmail_nonblocking_pipe_open(dial_pipe, NULL);
	if (err)
		return err;
	err = fd_listen(&dial_fhs, (re_sock_t)dial_pipe[0], FD_READ,
			 dial_pipe_handler, NULL);
	if (err) {
		close(dial_pipe[0]);
		close(dial_pipe[1]);
		dial_pipe[0] = dial_pipe[1] = -1;
		return err;
	}
	return 0;
}

static void report_queue_error(int err, const char *request_id,
			       const char *call_id)
{
	if (err == EAGAIN)
		emit_command_error(request_id, call_id, 429, "command queue full");
	else
		emit_command_error(request_id, call_id, 503,
				   "command queue unavailable");
}

static void dial_request(const char *request_id, const char *uri)
{
	int err;
	if (!str_isset(uri) || !str_isset(request_id))
		return;
	err = queue_command(VOXMAIL_COMMAND_DIAL, request_id, uri, NULL, 0, NULL);
	if (err) {
		warning("voxmail: dial command queue failed: %m\n", err);
		report_queue_error(err, request_id, NULL);
	}
}

static void baresip_event_handler(enum bevent_ev event, struct bevent *bevent,
						 void *arg)
{
	struct call *call;
	const char *id;
	int err;

	(void)arg;
	switch (event) {
	case BEVENT_REGISTERING:
	case BEVENT_REGISTER_OK:
	case BEVENT_REGISTER_FAIL:
	case BEVENT_UNREGISTERING:
		emit_registration_event(event, bevent);
		break;
	case BEVENT_CALL_INCOMING:
		(void)new_session(bevent_get_call(bevent), false);
		break;
	case BEVENT_CALL_OUTGOING:
		(void)new_session(bevent_get_call(bevent), true);
		break;
	case BEVENT_CALL_LOCAL_SDP:
		/* Baresip emits CALL_OUTGOING/CALL_INCOMING before allocating the
		 * media streams. Set the per-call device when LOCAL_SDP arrives,
		 * because call_audio() is guaranteed to exist at that point. */
		call = bevent_get_call(bevent);
		id = call ? call_id(call) : NULL;
		if (!call || !call_audio(call) || !str_isset(id))
			break;
		err = audio_set_devicename(call_audio(call), id, id);
		if (err)
			warning("voxmail: set per-call audio device failed: %m\n", err);
		break;
	default:
		break;
	}
}

static void handle_command(const char *buffer)
{
	struct odict *od = NULL;
	const struct odict_entry *entry;
	const char *value;
	char type[32];
	char id[128];
	char uri[DIAL_URI_MAX];
	int err;
	int64_t version;

	err = json_decode_odict(&od, 16, buffer, strlen(buffer), 16);
	if (err) {
		warning("voxmail: rejected malformed command (%m)\n", err);
		emit_command_error(NULL, NULL, 400, "malformed command");
		return;
	}
	entry = odict_lookup(od, "version");
	if (!entry || odict_entry_type(entry) != ODICT_INT ||
	    (version = odict_entry_int(entry)) != 2) {
		warning("voxmail: rejected command with unsupported protocol version\n");
		emit_command_error(NULL, NULL, 400, "unsupported protocol version");
		goto out;
	}
	value = odict_string(od, "type");
	if (!copy_json_string(value, type, sizeof(type))) {
		emit_command_error(NULL, NULL, 400, "missing or oversized command type");
		goto out;
	}
	if (0 == strcmp(type, "dial")) {
		char request_id[DIAL_ID_MAX];
		bool request_valid = copy_json_string(odict_string(od, "request_id"),
		                                      request_id, sizeof(request_id));
		bool target_valid = copy_json_string(odict_string(od, "to"), uri,
		                                    sizeof(uri));
		if (request_valid && target_valid)
			dial_request(request_id, uri);
		else {
			warning("voxmail: rejected dial with missing or oversized fields\n");
			emit_command_error(request_valid ? request_id : NULL, NULL, 400,
			                   "missing or oversized dial fields");
		}
		goto out;
	}
	if (!copy_json_string(odict_string(od, "call_id"), id, sizeof(id))) {
		warning("voxmail: rejected command with missing or oversized call_id\n");
		emit_command_error(NULL, NULL, 400,
		                   "missing or oversized call_id");
		goto out;
	}
	if (0 == strcmp(type, "hangup")) {
		int code = 603;
		int queue_err;
		int64_t raw_code;
		char reason[128] = "Caller not authorized";
		entry = odict_lookup(od, "code");
		if (entry && odict_entry_type(entry) != ODICT_INT) {
			warning("voxmail: rejected hangup with invalid code\n");
			emit_command_error(NULL, id, 400, "invalid hangup code");
			goto out;
		}
		if (entry && ((raw_code = odict_entry_int(entry)) < 100 ||
		              raw_code > 699)) {
			warning("voxmail: rejected hangup with invalid code\n");
			emit_command_error(NULL, id, 400, "invalid hangup code");
			goto out;
		}
		if (entry)
			code = (int)raw_code;
		entry = odict_lookup(od, "reason");
		if (entry && (odict_entry_type(entry) != ODICT_STRING ||
		              !copy_json_string(odict_string(od, "reason"), reason,
		                                sizeof(reason)))) {
			warning("voxmail: rejected hangup with invalid reason\n");
			emit_command_error(NULL, id, 400, "invalid hangup reason");
			goto out;
		}
		queue_err = queue_command(VOXMAIL_COMMAND_HANGUP, NULL, NULL, id, code, reason);
		if (queue_err) {
			warning("voxmail: hangup command queue failed: %m\n", queue_err);
			report_queue_error(queue_err, NULL, id);
		}
	}
	else if (0 == strcmp(type, "answer")) {
		int queue_err = queue_command(VOXMAIL_COMMAND_ANSWER, NULL, NULL, id, 0, NULL);
		if (queue_err) {
			warning("voxmail: answer command queue failed: %m\n", queue_err);
			report_queue_error(queue_err, NULL, id);
		}
	}
	else {
		warning("voxmail: rejected unknown command type\n");
		emit_command_error(NULL, id, 400, "unknown command type");
	}
out:
	mem_deref(od);
}

static void handle_frame(const char *frame, size_t length, void *arg)
{
	(void)length;
	(void)arg;
	handle_command(frame);
}

static void *socket_worker(void *arg)
{
	(void)arg;
	while (re_atomic_rlx(&running)) {
		struct pollfd listener = {server_fd, POLLIN, 0};
		int ready;
		do {
			ready = poll(&listener, 1, ACCEPT_POLL_TIMEOUT_MS);
		} while (ready < 0 && errno == EINTR);
		if (!re_atomic_rlx(&running))
			break;
		if (ready == 0)
			continue;
		if (ready < 0)
			break;
		if (listener.revents & (POLLERR | POLLHUP | POLLNVAL))
			continue;
		int fd = accept(server_fd, NULL, NULL);
		int old_fd = -1;
		uint64_t generation;
		if (fd < 0) {
			if (errno == EINTR)
				continue;
			if (errno == EAGAIN || errno == EWOULDBLOCK)
				continue;
			if (!re_atomic_rlx(&running))
				break;
			continue;
		}
		old_fd = voxmail_client_owner_publish(&client_owner, fd, &generation);
		if (old_fd >= 0)
			shutdown(old_fd, SHUT_RDWR);
		struct voxmail_frame_parser parser;
		voxmail_frame_parser_init(&parser);
		while (re_atomic_rlx(&running)) {
			char chunk[EVENT_BUFFER];
			ssize_t len = recv(fd, chunk, sizeof(chunk), 0);
			int frame_err;
			if (len <= 0)
				break;
			frame_err = voxmail_frame_parser_feed(&parser, chunk, (size_t)len,
							      handle_frame, NULL);
			if (frame_err) {
				warning("voxmail: rejecting control stream frame (%m)\n",
				        frame_err);
				break;
			}
		}
		(void)voxmail_client_owner_invalidate_if_current(&client_owner, fd,
		                                                generation);
		close(fd);
	}
	return NULL;
}

static int open_socket(void)
{
	struct sockaddr_un address;
	const char *path = getenv("VOXMAIL_CONTROL_SOCKET");
	int flags;
	if (!path || !*path || strlen(path) >= sizeof(socket_path))
		return EINVAL;
	strcpy(socket_path, path);
	unlink(socket_path);
	server_fd = socket(AF_UNIX, SOCK_STREAM, 0);
	if (server_fd < 0)
		return errno;
	flags = fcntl(server_fd, F_GETFL, 0);
	if (flags < 0 || fcntl(server_fd, F_SETFL, flags | O_NONBLOCK) < 0) {
		int err = errno;
		close(server_fd);
		server_fd = -1;
		return err;
	}
	memset(&address, 0, sizeof(address));
	address.sun_family = AF_UNIX;
	strcpy(address.sun_path, socket_path);
	if (bind(server_fd, (struct sockaddr *)&address, sizeof(address)) < 0 ||
		listen(server_fd, 8) < 0) {
		int err = errno;
		close(server_fd);
		server_fd = -1;
		unlink(socket_path);
		return err;
	}
	return 0;
}

static void module_cleanup(void)
{
	re_atomic_rlx_set(&running, false);
	if (command_queue.items)
		voxmail_command_queue_stop(&command_queue);
	if (bevent_started) {
		bevent_unregister(baresip_event_handler);
		bevent_started = false;
	}
	pcm_ausrc = mem_deref(pcm_ausrc);
	pcm_auplay = mem_deref(pcm_auplay);
	pthread_cond_broadcast(&event_cond);
	voxmail_client_owner_request_shutdown(&client_owner);
	if (socket_thread_started) {
		int join_err = voxmail_pthread_join(&socket_thread,
		                                   &socket_thread_started);
		if (join_err)
			warning("voxmail: socket thread join failed (%m)\n", join_err);
	}
	if (server_fd >= 0) {
		close(server_fd);
		server_fd = -1;
	}
	dial_fhs = mem_deref(dial_fhs);
	if (dial_pipe[0] >= 0)
		close(dial_pipe[0]);
	if (dial_pipe[1] >= 0)
		close(dial_pipe[1]);
	dial_pipe[0] = dial_pipe[1] = -1;
	if (event_thread_started) {
		int join_err = voxmail_pthread_join(&event_thread,
		                                   &event_thread_started);
		if (join_err)
			warning("voxmail: event thread join failed (%m)\n", join_err);
	}
	voxmail_command_queue_close(&command_queue);
	if (socket_path[0]) {
		(void)unlink(socket_path);
		socket_path[0] = '\0';
	}
	pthread_mutex_lock(&dial_lock);
	while (dial_queue.head) {
		struct dial_item *item = dial_queue.head->data;
		list_unlink(&item->le);
		mem_deref(item);
	}
	pthread_mutex_unlock(&dial_lock);
	pthread_mutex_lock(&event_lock);
	while (event_queue.head) {
		struct event_item *item = event_queue.head->data;
		list_unlink(&item->le);
		mem_deref(item);
	}
	event_count = 0;
	pthread_mutex_unlock(&event_lock);
	list_flush(&sessions);
}

static int module_init(void)
{
	int err;
	const char *root = getenv("VOXMAIL_AUDIO_DIR");

	if (root && strlen(root) < sizeof(audio_root))
		re_snprintf(audio_root, sizeof(audio_root), "%s", root);
	(void)signal(SIGPIPE, SIG_IGN);
	list_init(&sessions);
	list_init(&dial_queue);
	list_init(&event_queue);
	event_count = 0;
	dial_pipe[0] = dial_pipe[1] = -1;
	server_fd = -1;
	socket_thread_started = false;
	event_thread_started = false;
	bevent_started = false;
	re_atomic_rlx_set(&running, false);
	if (mkdir(audio_root, 0700) < 0 && errno != EEXIST) {
		err = errno;
		goto fail;
	}
	{
		struct stat st;
		if (stat(audio_root, &st) < 0)
			err = errno;
		else if (!S_ISDIR(st.st_mode))
			err = ENOTDIR;
		else
			err = 0;
		if (err)
			goto fail;
	}
	err = ausrc_register(&pcm_ausrc, baresip_ausrcl(), "voxmail", pcm_source_alloc);
	if (err)
		goto fail;
	err = auplay_register(&pcm_auplay, baresip_auplayl(), "voxmail", pcm_player_alloc);
	if (err)
		goto fail;
	err = open_socket();
	if (err)
		goto fail;
	err = dial_open();
	if (err)
		goto fail;
	err = voxmail_command_queue_init(&command_queue, COMMAND_QUEUE_MAX);
	if (err)
		goto fail;
	err = bevent_register(baresip_event_handler, 0);
	if (err)
		goto fail;
	bevent_started = true;
	re_atomic_rlx_set(&running, true);
	err = voxmail_pthread_start(&event_thread, &event_thread_started, NULL,
	                            event_worker, NULL, NULL, NULL);
	if (err)
		goto fail;
	err = voxmail_pthread_start(&socket_thread, &socket_thread_started, NULL,
	                            socket_worker, NULL, NULL, NULL);
	if (err)
		goto fail;
	return 0;

fail:
	module_cleanup();
	return err;
}

static int module_close(void)
{
	module_cleanup();
	return 0;
}

const struct mod_export DECL_EXPORTS(voxmail) = {
	"voxmail", "application", module_init, module_close
};
