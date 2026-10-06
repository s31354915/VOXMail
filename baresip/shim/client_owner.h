#ifndef VOXMAIL_CLIENT_OWNER_H
#define VOXMAIL_CLIENT_OWNER_H

#include <pthread.h>
#include <stdbool.h>
#include <stdint.h>

struct voxmail_client_owner {
	pthread_mutex_t lock;
	int fd;
	uint64_t generation;
};

#define VOXMAIL_CLIENT_OWNER_INITIALIZER \
	{ PTHREAD_MUTEX_INITIALIZER, -1, 0 }

void voxmail_client_owner_init(struct voxmail_client_owner *owner);
void voxmail_client_owner_destroy(struct voxmail_client_owner *owner);

/* Publish a newly accepted descriptor and return the previous descriptor. */
int voxmail_client_owner_publish(struct voxmail_client_owner *owner, int fd,
					 uint64_t *generation);

/* Invalidate the published descriptor and wake its receiver; never close it. */
void voxmail_client_owner_request_shutdown(struct voxmail_client_owner *owner);

/* Duplicate the current descriptor while taking a consistent generation. */
int voxmail_client_owner_snapshot_dup(struct voxmail_client_owner *owner,
					      int *source_fd, uint64_t *generation,
					      int *duplicate_fd);

/* Invalidate only if both descriptor and generation still match. */
bool voxmail_client_owner_invalidate_if_current(
		struct voxmail_client_owner *owner, int source_fd, uint64_t generation);

#endif
