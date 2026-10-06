#ifndef VOXMAIL_PCM_WRITER_H
#define VOXMAIL_PCM_WRITER_H

#include <stddef.h>

struct voxmail_pcm_writer {
	size_t written;
	size_t max_bytes;
};

void voxmail_pcm_writer_init(struct voxmail_pcm_writer *writer,
				     size_t max_bytes);
int voxmail_pcm_writer_write(struct voxmail_pcm_writer *writer, int fd,
				     const void *data, size_t len);

#endif
