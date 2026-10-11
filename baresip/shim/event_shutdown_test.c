#include "event_shutdown.h"

#include <assert.h>
#include <errno.h>
#include <stdbool.h>
#include <stdatomic.h>
#include <time.h>

struct fixture {
	pthread_mutex_t event_lock;
	pthread_cond_t event_cond;
	pthread_mutex_t gate_lock;
	pthread_cond_t gate_cond;
	atomic_bool running;
	bool predicate_checked;
	bool enter_wait;
	bool stop_attempted;
	int wait_result;
	bool exited;
};

static void initialize(struct fixture *f)
{
	pthread_mutexattr_t attr;

	assert(pthread_mutexattr_init(&attr) == 0);
	/* An error-checking mutex lets stop_running prove that the caller owns
	 * the predicate mutex, independently of scheduling or timing. */
	assert(pthread_mutexattr_settype(&attr, PTHREAD_MUTEX_ERRORCHECK) == 0);
	assert(pthread_mutex_init(&f->event_lock, &attr) == 0);
	assert(pthread_mutexattr_destroy(&attr) == 0);
	assert(pthread_cond_init(&f->event_cond, NULL) == 0);
	assert(pthread_mutex_init(&f->gate_lock, NULL) == 0);
	assert(pthread_cond_init(&f->gate_cond, NULL) == 0);
	atomic_init(&f->running, true);
	f->predicate_checked = f->enter_wait = f->stop_attempted = false;
	f->wait_result = 0;
	f->exited = false;
}

static void destroy(struct fixture *f)
{
	assert(pthread_mutex_destroy(&f->event_lock) == 0);
	assert(pthread_cond_destroy(&f->event_cond) == 0);
	assert(pthread_mutex_destroy(&f->gate_lock) == 0);
	assert(pthread_cond_destroy(&f->gate_cond) == 0);
}

static void stop_running(void *arg)
{
	struct fixture *f = arg;

	/* This returns EPERM if shutdown invokes us without owning event_lock.
	 * Reacquire before changing the predicate; only the test checks ownership
	 * this way, the production callback is just its atomic running=false. */
	assert(pthread_mutex_unlock(&f->event_lock) == 0);
	assert(pthread_mutex_lock(&f->event_lock) == 0);
	atomic_store(&f->running, false);
}

static void *worker(void *arg)
{
	struct fixture *f = arg;
	struct timespec deadline;

	assert(pthread_mutex_lock(&f->event_lock) == 0);
	assert(atomic_load(&f->running));
	assert(pthread_mutex_lock(&f->gate_lock) == 0);
	f->predicate_checked = true;
	assert(pthread_cond_broadcast(&f->gate_cond) == 0);
	while (!f->enter_wait)
		assert(pthread_cond_wait(&f->gate_cond, &f->gate_lock) == 0);
	assert(pthread_mutex_unlock(&f->gate_lock) == 0);

	/* Freeze exactly after the production loop read running=true and before
	 * pthread_cond_wait atomically drops event_lock. The legacy test supplies
	 * a broadcast during that gap; the fixed stop must wait for this mutex. */
	assert(clock_gettime(CLOCK_REALTIME, &deadline) == 0);
	deadline.tv_sec += 1;
	do {
		f->wait_result = pthread_cond_timedwait(&f->event_cond,
		                                      &f->event_lock, &deadline);
	} while (!f->wait_result && atomic_load(&f->running));
	f->exited = !atomic_load(&f->running);
	assert(pthread_mutex_unlock(&f->event_lock) == 0);
	return NULL;
}

static void wait_for_predicate(struct fixture *f)
{
	assert(pthread_mutex_lock(&f->gate_lock) == 0);
	while (!f->predicate_checked)
		assert(pthread_cond_wait(&f->gate_cond, &f->gate_lock) == 0);
	assert(pthread_mutex_unlock(&f->gate_lock) == 0);
}

static void allow_wait(struct fixture *f)
{
	assert(pthread_mutex_lock(&f->gate_lock) == 0);
	f->enter_wait = true;
	assert(pthread_cond_broadcast(&f->gate_cond) == 0);
	assert(pthread_mutex_unlock(&f->gate_lock) == 0);
}

static void *shutdown_worker(void *arg)
{
	struct fixture *f = arg;

	assert(pthread_mutex_lock(&f->gate_lock) == 0);
	f->stop_attempted = true;
	assert(pthread_cond_broadcast(&f->gate_cond) == 0);
	assert(pthread_mutex_unlock(&f->gate_lock) == 0);
	assert(voxmail_event_shutdown(&f->event_lock, &f->event_cond,
	                             stop_running, f) == 0);
	return NULL;
}

static void test_old_interleaving_loses_the_broadcast(void)
{
	struct fixture f;
	pthread_t waiter;

	initialize(&f);
	assert(pthread_create(&waiter, NULL, worker, &f) == 0);
	wait_for_predicate(&f);
	/* Reproduce the original cleanup: predicate transition + broadcast while
	 * the waiter still owns event_lock, followed by entering the wait. */
	atomic_store(&f.running, false);
	assert(pthread_cond_broadcast(&f.event_cond) == 0);
	allow_wait(&f);
	assert(pthread_join(waiter, NULL) == 0);
	assert(f.wait_result == ETIMEDOUT);
	destroy(&f);
}

static void test_shutdown_during_predicate_to_wait_transition(void)
{
	struct fixture f;
	pthread_t waiter, stopper;

	initialize(&f);
	assert(pthread_create(&waiter, NULL, worker, &f) == 0);
	wait_for_predicate(&f);
	assert(pthread_create(&stopper, NULL, shutdown_worker, &f) == 0);
	assert(pthread_mutex_lock(&f.gate_lock) == 0);
	while (!f.stop_attempted)
		assert(pthread_cond_wait(&f.gate_cond, &f.gate_lock) == 0);
	assert(pthread_mutex_unlock(&f.gate_lock) == 0);
	allow_wait(&f);
	assert(pthread_join(stopper, NULL) == 0);
	assert(pthread_join(waiter, NULL) == 0);
	assert(f.wait_result == 0 && f.exited);
	/* A repeated cleanup also remains valid after workers have exited. */
	assert(voxmail_event_shutdown(&f.event_lock, &f.event_cond,
	                             stop_running, &f) == 0);
	destroy(&f);
}

int main(void)
{
	int i;

	test_old_interleaving_loses_the_broadcast();
	for (i = 0; i < 1000; ++i)
		test_shutdown_during_predicate_to_wait_transition();
	return 0;
}
