#include "command_queue.h"

#include <errno.h>
#include <stdlib.h>
#include <string.h>

int voxmail_command_queue_init(struct voxmail_command_queue *queue,
				       size_t capacity)
{
	int err;

	if (!queue || capacity == 0)
		return EINVAL;
	memset(queue, 0, sizeof(*queue));
	queue->items = calloc(capacity, sizeof(*queue->items));
	if (!queue->items)
		return ENOMEM;
	err = pthread_mutex_init(&queue->lock, NULL);
	if (err) {
		free(queue->items);
		memset(queue, 0, sizeof(*queue));
		return err;
	}
	queue->capacity = capacity;
	queue->accepting = true;
	return 0;
}

void voxmail_command_queue_stop(struct voxmail_command_queue *queue)
{
	if (!queue)
		return;
	pthread_mutex_lock(&queue->lock);
	queue->accepting = false;
	pthread_mutex_unlock(&queue->lock);
}

int voxmail_command_queue_push(struct voxmail_command_queue *queue,
				       const struct voxmail_command *command)
{
	size_t index;
	int err = 0;

	if (!queue || !command)
		return EINVAL;
	pthread_mutex_lock(&queue->lock);
	if (!queue->accepting)
		err = EPIPE;
	else if (queue->count == queue->capacity)
		err = EAGAIN;
	else {
		index = (queue->head + queue->count) % queue->capacity;
		queue->items[index] = *command;
		++queue->count;
	}
	pthread_mutex_unlock(&queue->lock);
	return err;
}

bool voxmail_command_queue_pop(struct voxmail_command_queue *queue,
				       struct voxmail_command *command)
{
	bool found = false;

	if (!queue || !command)
		return false;
	pthread_mutex_lock(&queue->lock);
	if (queue->count > 0) {
		*command = queue->items[queue->head];
		queue->head = (queue->head + 1) % queue->capacity;
		--queue->count;
		found = true;
	}
	pthread_mutex_unlock(&queue->lock);
	return found;
}

bool voxmail_command_queue_pending(struct voxmail_command_queue *queue)
{
	bool pending;

	if (!queue)
		return false;
	pthread_mutex_lock(&queue->lock);
	pending = queue->count != 0;
	pthread_mutex_unlock(&queue->lock);
	return pending;
}

size_t voxmail_command_queue_dispatch(struct voxmail_command_queue *queue,
					      size_t limit,
					      voxmail_command_handler_h *handler,
					      void *arg)
{
	struct voxmail_command command;
	size_t processed = 0;

	if (!queue || limit == 0 || !handler)
		return 0;
	while (processed < limit &&
	       voxmail_command_queue_pop(queue, &command)) {
		handler(&command, arg);
		++processed;
	}
	return processed;
}

void voxmail_command_queue_close(struct voxmail_command_queue *queue)
{
	if (!queue || !queue->items)
		return;
	voxmail_command_queue_stop(queue);
	pthread_mutex_destroy(&queue->lock);
	free(queue->items);
	memset(queue, 0, sizeof(*queue));
}
