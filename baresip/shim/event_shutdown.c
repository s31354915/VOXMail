#include "event_shutdown.h"

#include <errno.h>

int voxmail_event_shutdown(pthread_mutex_t *lock, pthread_cond_t *condition,
			  voxmail_event_stop_h *stop, void *arg)
{
	int err, unlock_err;

	if (!lock || !condition || !stop)
		return EINVAL;
	err = pthread_mutex_lock(lock);
	if (err)
		return err;
	stop(arg);
	err = pthread_cond_broadcast(condition);
	unlock_err = pthread_mutex_unlock(lock);
	return err ? err : unlock_err;
}
