#include "command_wakeup.h"

#include <errno.h>
#include <unistd.h>

int voxmail_command_wakeup(int fd)
{
	char wake = 1;
	ssize_t written;

	if (fd < 0)
		return EBADF;
	do {
		written = write(fd, &wake, sizeof(wake));
	} while (written < 0 && errno == EINTR);
	if (written == (ssize_t)sizeof(wake) ||
	    (written < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)))
		return 0;
	if (written < 0)
		return errno;
	return EPIPE;
}
