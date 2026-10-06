#include "command_wakeup.h"

#include <assert.h>
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <unistd.h>

static void test_closed_pipe(void)
{
	int pipefd[2];

	assert(pipe(pipefd) == 0);
	close(pipefd[0]);
	assert(voxmail_command_wakeup(pipefd[1]) == EPIPE);
	close(pipefd[1]);
	assert(voxmail_command_wakeup(-1) == EBADF);
}

static void test_full_nonblocking_pipe_is_a_pending_wakeup(void)
{
	int pipefd[2];
	int flags;
	char byte = 1;

	assert(pipe(pipefd) == 0);
	flags = fcntl(pipefd[1], F_GETFL, 0);
	assert(flags >= 0);
	assert(fcntl(pipefd[1], F_SETFL, flags | O_NONBLOCK) == 0);
	while (write(pipefd[1], &byte, sizeof(byte)) == 1)
		;
	assert(voxmail_command_wakeup(pipefd[1]) == 0);
	close(pipefd[0]);
	close(pipefd[1]);
}

int main(void)
{
	(void)signal(SIGPIPE, SIG_IGN);
	test_closed_pipe();
	test_full_nonblocking_pipe_is_a_pending_wakeup();
	return 0;
}
