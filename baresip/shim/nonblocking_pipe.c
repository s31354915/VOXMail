#include "nonblocking_pipe.h"

#include <errno.h>
#include <fcntl.h>
#include <unistd.h>

static int default_pipe_create(int pipefd[2], void *arg)
{
	(void)arg;
	return pipe(pipefd);
}

static int default_pipe_fcntl(int fd, int command, int value, void *arg)
{
	(void)arg;
	return fcntl(fd, command, value);
}

int voxmail_nonblocking_pipe_open(int pipefd[2],
					 const struct voxmail_pipe_ops *ops)
{
	voxmail_pipe_create_h *create = default_pipe_create;
	voxmail_pipe_fcntl_h *set_fcntl = default_pipe_fcntl;
	void *arg = NULL;
	int flags;
	int err;

	if (!pipefd)
		return EINVAL;
	pipefd[0] = pipefd[1] = -1;
	if (ops) {
		if (ops->create)
			create = ops->create;
		if (ops->fcntl)
			set_fcntl = ops->fcntl;
		arg = ops->arg;
	}
	if (create(pipefd, arg) < 0)
		return errno;
	flags = set_fcntl(pipefd[0], F_GETFL, 0, arg);
	if (flags < 0 || set_fcntl(pipefd[0], F_SETFL, flags | O_NONBLOCK, arg) < 0) {
		err = errno;
		goto fail;
	}
	flags = set_fcntl(pipefd[1], F_GETFL, 0, arg);
	if (flags < 0 || set_fcntl(pipefd[1], F_SETFL, flags | O_NONBLOCK, arg) < 0) {
		err = errno;
		goto fail;
	}
	return 0;

fail:
	close(pipefd[0]);
	close(pipefd[1]);
	pipefd[0] = pipefd[1] = -1;
	return err;
}
