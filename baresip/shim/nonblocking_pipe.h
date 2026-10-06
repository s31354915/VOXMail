#ifndef VOXMAIL_NONBLOCKING_PIPE_H
#define VOXMAIL_NONBLOCKING_PIPE_H

#include <stddef.h>

typedef int(voxmail_pipe_create_h)(int pipefd[2], void *arg);
typedef int(voxmail_pipe_fcntl_h)(int fd, int command, int value, void *arg);

struct voxmail_pipe_ops {
	voxmail_pipe_create_h *create;
	voxmail_pipe_fcntl_h *fcntl;
	void *arg;
};

/* Open both ends nonblocking; on every failure both outputs are closed. */
int voxmail_nonblocking_pipe_open(int pipefd[2],
					 const struct voxmail_pipe_ops *ops);

#endif
