#include "pthread_start.h"

#include <errno.h>

static int default_create(pthread_t *thread, const pthread_attr_t *attributes,
				  void *(*start)(void *), void *arg, void *hook_arg)
{
	(void)hook_arg;
	return pthread_create(thread, attributes, start, arg);
}

int voxmail_pthread_start(pthread_t *thread, bool *started,
				  const pthread_attr_t *attributes,
				  void *(*start)(void *), void *arg,
				  voxmail_pthread_create_h *create, void *hook_arg)
{
	int err;

	if (!thread || !started || !start)
		return EINVAL;
	if (*started)
		return EBUSY;
	if (!create)
		create = default_create;
	err = create(thread, attributes, start, arg, hook_arg);
	if (err)
		return err;
	*started = true;
	return 0;
}

int voxmail_pthread_join(pthread_t *thread, bool *started)
{
	int err;

	if (!thread || !started)
		return EINVAL;
	if (!*started)
		return 0;
	err = pthread_join(*thread, NULL);
	if (!err)
		*started = false;
	return err;
}
