#ifndef VOXMAIL_COMMAND_QUEUE_H
#define VOXMAIL_COMMAND_QUEUE_H

#include <stdbool.h>
#include <stddef.h>
#include <pthread.h>

enum voxmail_command_type {
	VOXMAIL_COMMAND_DIAL,
	VOXMAIL_COMMAND_ANSWER,
	VOXMAIL_COMMAND_HANGUP,
};

struct voxmail_command {
	enum voxmail_command_type type;
	char request_id[128];
	char uri[256];
	char call_id[128];
	int code;
	char reason[128];
};

struct voxmail_command_queue {
	struct voxmail_command *items;
	size_t capacity;
	size_t head;
	size_t count;
	bool accepting;
	pthread_mutex_t lock;
};

typedef void (voxmail_command_handler_h)(const struct voxmail_command *command,
						void *arg);

int voxmail_command_queue_init(struct voxmail_command_queue *queue,
				       size_t capacity);
void voxmail_command_queue_stop(struct voxmail_command_queue *queue);
int voxmail_command_queue_push(struct voxmail_command_queue *queue,
				       const struct voxmail_command *command);
bool voxmail_command_queue_pop(struct voxmail_command_queue *queue,
				       struct voxmail_command *command);
size_t voxmail_command_queue_dispatch(struct voxmail_command_queue *queue,
					      size_t limit,
					      voxmail_command_handler_h *handler,
					      void *arg);
bool voxmail_command_queue_pending(struct voxmail_command_queue *queue);
void voxmail_command_queue_close(struct voxmail_command_queue *queue);

#endif
