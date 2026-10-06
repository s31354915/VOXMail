#include "pcm_writer.h"

#include <errno.h>
#include <stdint.h>
#include <unistd.h>

enum { VOXMAIL_PCM_SAMPLE_BYTES = 2 };

void voxmail_pcm_writer_init(struct voxmail_pcm_writer *writer,
				     size_t max_bytes)
{
	if (!writer)
		return;
	writer->written = 0;
	writer->max_bytes = max_bytes - max_bytes % VOXMAIL_PCM_SAMPLE_BYTES;
}

int voxmail_pcm_writer_write(struct voxmail_pcm_writer *writer, int fd,
				     const void *data, size_t len)
{
	const unsigned char *bytes = data;
	size_t limit;
	size_t sent = 0;

	if (!writer || fd < 0 || (!data && len != 0))
		return EINVAL;
	if (writer->written >= writer->max_bytes || len == 0)
		return 0;
	limit = writer->max_bytes - writer->written;
	if (limit > len)
		limit = len;
	limit -= limit % VOXMAIL_PCM_SAMPLE_BYTES;
	while (sent < limit) {
		ssize_t n = write(fd, bytes + sent, limit - sent);
		if (n > 0) {
			sent += (size_t)n;
			writer->written += (size_t)n;
			continue;
		}
		if (n < 0 && errno == EINTR)
			continue;
		return n < 0 ? errno : EIO;
	}
	return 0;
}
