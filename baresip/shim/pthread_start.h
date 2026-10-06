#ifndef VOXMAIL_PTHREAD_START_H
#define VOXMAIL_PTHREAD_START_H

#include <stdbool.h>
#include <pthread.h>

typedef int(voxmail_pthread_create_h)(pthread_t *thread,
					      const pthread_attr_t *attributes,
					      void *(*start)(void *), void *arg, void *hook_arg);

int voxmail_pthread_start(pthread_t *thread, bool *started,
				  const pthread_attr_t *attributes,
				  void *(*start)(void *), void *arg,
				  voxmail_pthread_create_h *create, void *hook_arg);

int voxmail_pthread_join(pthread_t *thread, bool *started);

#endif
