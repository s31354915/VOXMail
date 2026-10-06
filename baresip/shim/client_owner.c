#include "client_owner.h"

#include <errno.h>
#include <sys/socket.h>
#include <unistd.h>

static void advance_generation(struct voxmail_client_owner *owner)
{
	++owner->generation;
	if (owner->generation == 0)
		++owner->generation;
}

void voxmail_client_owner_init(struct voxmail_client_owner *owner)
{
	if (!owner)
		return;
	(void)pthread_mutex_init(&owner->lock, NULL);
	owner->fd = -1;
	owner->generation = 0;
}

void voxmail_client_owner_destroy(struct voxmail_client_owner *owner)
{
	if (!owner)
		return;
	(void)pthread_mutex_destroy(&owner->lock);
}

int voxmail_client_owner_publish(struct voxmail_client_owner *owner, int fd,
					 uint64_t *generation)
{
	int old_fd;

	if (!owner || fd < 0)
		return -1;
	pthread_mutex_lock(&owner->lock);
	old_fd = owner->fd;
	owner->fd = fd;
	advance_generation(owner);
	if (generation)
		*generation = owner->generation;
	pthread_mutex_unlock(&owner->lock);
	return old_fd;
}

void voxmail_client_owner_request_shutdown(struct voxmail_client_owner *owner)
{
	if (!owner)
		return;
	pthread_mutex_lock(&owner->lock);
	if (owner->fd >= 0) {
		(void)shutdown(owner->fd, SHUT_RDWR);
		owner->fd = -1;
		advance_generation(owner);
	}
	pthread_mutex_unlock(&owner->lock);
}

int voxmail_client_owner_snapshot_dup(struct voxmail_client_owner *owner,
					      int *source_fd, uint64_t *generation,
					      int *duplicate_fd)
{
	int duplicate;

	if (!owner || !source_fd || !generation || !duplicate_fd)
		return EINVAL;
	*source_fd = -1;
	*duplicate_fd = -1;
	pthread_mutex_lock(&owner->lock);
	if (owner->fd < 0) {
		pthread_mutex_unlock(&owner->lock);
		return ENOTCONN;
	}
	duplicate = dup(owner->fd);
	if (duplicate < 0) {
		int err = errno;
		pthread_mutex_unlock(&owner->lock);
		return err;
	}
	*source_fd = owner->fd;
	*generation = owner->generation;
	*duplicate_fd = duplicate;
	pthread_mutex_unlock(&owner->lock);
	return 0;
}

bool voxmail_client_owner_invalidate_if_current(
		struct voxmail_client_owner *owner, int source_fd, uint64_t generation)
{
	bool invalidated = false;

	if (!owner)
		return false;
	pthread_mutex_lock(&owner->lock);
	if (owner->fd == source_fd && owner->generation == generation) {
		owner->fd = -1;
		advance_generation(owner);
		invalidated = true;
	}
	pthread_mutex_unlock(&owner->lock);
	return invalidated;
}
