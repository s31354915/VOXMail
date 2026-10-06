#include "pthread_start.h"

#include <assert.h>
#include <errno.h>

struct failure {
	int error;
};

static int fail_create(pthread_t *thread, const pthread_attr_t *attributes,
			       void *(*start)(void *), void *arg, void *hook_arg)
{
	struct failure *failure = hook_arg;
	(void)thread;
	(void)attributes;
	(void)start;
	(void)arg;
	return failure->error;
}

static void *return_arg(void *arg)
{
	return arg;
}

static void test_creation_failure_never_marks_started(void)
{
	struct failure failure = {.error = EAGAIN};
	pthread_t thread;
	bool started = false;

	assert(voxmail_pthread_start(&thread, &started, NULL, return_arg, NULL,
	                             fail_create, &failure) == EAGAIN);
	assert(!started);
	assert(voxmail_pthread_join(&thread, &started) == 0);
}

static void test_success_is_joinable_once(void)
{
	pthread_t thread;
	bool started = false;

	assert(voxmail_pthread_start(&thread, &started, NULL, return_arg, NULL,
	                             NULL, NULL) == 0);
	assert(started);
	assert(voxmail_pthread_join(&thread, &started) == 0);
	assert(!started);
	assert(voxmail_pthread_join(&thread, &started) == 0);
}

int main(void)
{
	test_creation_failure_never_marks_started();
	test_success_is_joinable_once();
	return 0;
}
