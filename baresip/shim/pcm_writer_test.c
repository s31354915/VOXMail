#include "pcm_writer.h"

#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/stat.h>
#include <unistd.h>

static void check(int condition, const char *message)
{
	if (!condition) {
		fprintf(stderr, "%s\n", message);
		exit(EXIT_FAILURE);
	}
}

int main(void)
{
	char path[] = "/tmp/voxmail-pcm-writer-XXXXXX";
	struct voxmail_pcm_writer writer;
	unsigned char samples[10] = {0, 1, 2, 3, 4, 5, 6, 7, 8, 9};
	struct stat st;
	int fd = mkstemp(path);
	check(fd >= 0, "mkstemp failed");
	voxmail_pcm_writer_init(&writer, 10);
	check(voxmail_pcm_writer_write(&writer, fd, samples, sizeof(samples)) == 0,
	      "first bounded write failed");
	check(writer.written == 10, "writer did not stop at the even byte cap");
	check(voxmail_pcm_writer_write(&writer, fd, samples, sizeof(samples)) == 0,
	      "capped write was not harmless");
	check(writer.written == 10, "writer exceeded its cap");
	check(fstat(fd, &st) == 0 && st.st_size == 10,
	      "capture file exceeded its bounded size");
	check(voxmail_pcm_writer_write(&writer, -1, samples, 1) == EINVAL,
	      "invalid descriptor was not rejected");
	close(fd);
	unlink(path);
	return 0;
}
