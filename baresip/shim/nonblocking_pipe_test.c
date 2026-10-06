#include "nonblocking_pipe.h"

#include <assert.h>
#include <errno.h>
#include <fcntl.h>
#include <unistd.h>

struct failure {
	int create_error;
	int fcntl_call;
	int fail_call;
	int fail_error;
};

static int fail_create(int pipefd[2], void *arg)
{
	struct failure *failure = arg;
	(void)pipefd;
	errno = failure->create_error;
	return -1;
}

static int fail_fcntl(int fd, int command, int value, void *arg)
{
	struct failure *failure = arg;
	++failure->fcntl_call;
	if (failure->fcntl_call == failure->fail_call) {
		errno = failure->fail_error;
		return -1;
	}
	return fcntl(fd, command, value);
}

static void test_real_pipe(void)
{
	int pipefd[2];
	int flags;

	assert(voxmail_nonblocking_pipe_open(pipefd, NULL) == 0);
	flags = fcntl(pipefd[0], F_GETFL, 0);
	assert(flags >= 0 && (flags & O_NONBLOCK));
	flags = fcntl(pipefd[1], F_GETFL, 0);
	assert(flags >= 0 && (flags & O_NONBLOCK));
	close(pipefd[0]);
	close(pipefd[1]);
}

static void test_pipe_failure_injection(void)
{
	struct failure failure = {.create_error = EMFILE};
	struct voxmail_pipe_ops ops = {fail_create, NULL, &failure};
	int pipefd[2] = {123, 456};

	assert(voxmail_nonblocking_pipe_open(pipefd, &ops) == EMFILE);
	assert(pipefd[0] == -1 && pipefd[1] == -1);

	failure = (struct failure){.fail_call = 1, .fail_error = EACCES};
	ops.create = NULL;
	ops.fcntl = fail_fcntl;
	assert(voxmail_nonblocking_pipe_open(pipefd, &ops) == EACCES);
	assert(pipefd[0] == -1 && pipefd[1] == -1);

	failure = (struct failure){.fail_call = 4, .fail_error = ENOSPC};
	assert(voxmail_nonblocking_pipe_open(pipefd, &ops) == ENOSPC);
	assert(pipefd[0] == -1 && pipefd[1] == -1);
}

int main(void)
{
	test_real_pipe();
	test_pipe_failure_injection();
	return 0;
}
