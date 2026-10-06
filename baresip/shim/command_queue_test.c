#include "command_queue.h"
#include "call_dispatch.h"

#include <assert.h>
#include <errno.h>
#include <pthread.h>
#include <stddef.h>
#include <string.h>

struct producer_args {
	struct voxmail_command_queue *queue;
	size_t stopped;
};

struct fake_call {
	bool open;
	int answers;
	int hangups;
	int closes;
	int releases;
};

static void *fake_find(void *arg, const char *call_id)
{
	struct fake_call *call = arg;
	return call->open && !strcmp(call_id, "closed-call") ? call : NULL;
}

static int fake_answer(void *arg, void *handle)
{
	struct fake_call *call = handle;
	(void)arg;
	++call->answers;
	return 0;
}

static int fake_hangup(void *arg, void *handle, int code, const char *reason)
{
	struct fake_call *call = handle;
	(void)arg;
	(void)code;
	(void)reason;
	++call->hangups;
	call->open = false;
	return 0;
}

static void fake_close(void *arg, void *handle)
{
	struct fake_call *call = handle;
	(void)arg;
	++call->closes;
}

static void fake_release(void *arg, void *handle)
{
	struct fake_call *call = handle;
	(void)arg;
	++call->releases;
}

static const struct voxmail_call_ops fake_ops = {
	fake_find, fake_answer, fake_hangup, fake_close, fake_release,
};

static void *push_after_stop(void *arg)
{
	struct producer_args *args = arg;
	struct voxmail_command command = {0};
	size_t i;

	command.type = VOXMAIL_COMMAND_HANGUP;
	for (i = 0; i < 10000; ++i) {
		if (voxmail_command_queue_push(args->queue, &command) == EPIPE)
			++args->stopped;
	}
	return NULL;
}

static void count_dispatched(const struct voxmail_command *command, void *arg)
{
	size_t *count = arg;
	assert(command != NULL);
	++*count;
}

static void test_order_and_capacity(void)
{
	struct voxmail_command_queue queue;
	struct voxmail_command command = {0};
	struct voxmail_command got = {0};

	assert(voxmail_command_queue_init(&queue, 3) == 0);
	command.type = VOXMAIL_COMMAND_DIAL;
	strcpy(command.request_id, "one");
	assert(voxmail_command_queue_push(&queue, &command) == 0);
	command.type = VOXMAIL_COMMAND_ANSWER;
	strcpy(command.call_id, "two");
	assert(voxmail_command_queue_push(&queue, &command) == 0);
	command.type = VOXMAIL_COMMAND_HANGUP;
	strcpy(command.call_id, "three");
	assert(voxmail_command_queue_push(&queue, &command) == 0);
	assert(voxmail_command_queue_push(&queue, &command) == EAGAIN);

	assert(voxmail_command_queue_pop(&queue, &got));
	assert(got.type == VOXMAIL_COMMAND_DIAL && !strcmp(got.request_id, "one"));
	assert(voxmail_command_queue_pop(&queue, &got));
	assert(got.type == VOXMAIL_COMMAND_ANSWER && !strcmp(got.call_id, "two"));
	assert(voxmail_command_queue_pop(&queue, &got));
	assert(got.type == VOXMAIL_COMMAND_HANGUP && !strcmp(got.call_id, "three"));
	assert(!voxmail_command_queue_pending(&queue));
	assert(!voxmail_command_queue_pop(&queue, &got));
	voxmail_command_queue_close(&queue);

	assert(voxmail_command_queue_init(&queue, 64) == 0);
	for (size_t i = 0; i < 33; ++i)
		assert(voxmail_command_queue_push(&queue, &command) == 0);
	{
		size_t dispatched = 0;
		assert(voxmail_command_queue_dispatch(&queue, 32, count_dispatched,
		                                      &dispatched) == 32);
		assert(dispatched == 32 && voxmail_command_queue_pending(&queue));
	}
	{
		size_t dispatched = 0;
		assert(voxmail_command_queue_dispatch(&queue, 32, count_dispatched,
		                                      &dispatched) == 1);
		assert(dispatched == 1 && !voxmail_command_queue_pending(&queue));
	}
	voxmail_command_queue_close(&queue);
}

static void test_repeated_stale_hangups_and_shutdown(void)
{
	struct voxmail_command_queue queue;
	struct voxmail_command command = {0};
	struct fake_call call = {.open = true};
	struct producer_args args;
	pthread_t producer;

	assert(voxmail_command_queue_init(&queue, 8) == 0);
	command.type = VOXMAIL_COMMAND_HANGUP;
	strcpy(command.call_id, "closed-call");
	assert(voxmail_command_queue_push(&queue, &command) == 0);
	assert(voxmail_command_queue_push(&queue, &command) == 0);
	/* The native dispatcher resolves the call at execution time. Once the
	 * first hangup closes it, the second is a harmless stale command. */
	assert(voxmail_command_queue_pop(&queue, &command));
	assert(voxmail_dispatch_call(&command, &fake_ops, &call) ==
	       VOXMAIL_DISPATCH_EXECUTED);
	assert(call.hangups == 1 && call.closes == 1 && call.releases == 1);
	assert(voxmail_command_queue_pop(&queue, &command));
	assert(voxmail_dispatch_call(&command, &fake_ops, &call) ==
	       VOXMAIL_DISPATCH_STALE);
	assert(call.hangups == 1 && call.closes == 1 && call.releases == 1);

	command.type = VOXMAIL_COMMAND_ANSWER;
	assert(voxmail_dispatch_call(&command, &fake_ops, &call) ==
	       VOXMAIL_DISPATCH_STALE);
	assert(call.answers == 0);

	args.queue = &queue;
	args.stopped = 0;
	voxmail_command_queue_stop(&queue);
	assert(pthread_create(&producer, NULL, push_after_stop, &args) == 0);
	assert(pthread_join(producer, NULL) == 0);
	assert(args.stopped == 10000);
	voxmail_command_queue_close(&queue);
}

int main(void)
{
	test_order_and_capacity();
	test_repeated_stale_hangups_and_shutdown();
	return 0;
}
