#ifndef VOXMAIL_EVENT_SHUTDOWN_H
#define VOXMAIL_EVENT_SHUTDOWN_H

#include <pthread.h>

/* Change the worker predicate and broadcast under the same mutex used by
 * pthread_cond_wait. stop() must not acquire that mutex itself. */
typedef void (voxmail_event_stop_h)(void *arg);
int voxmail_event_shutdown(pthread_mutex_t *lock, pthread_cond_t *condition,
			  voxmail_event_stop_h *stop, void *arg);

#endif
