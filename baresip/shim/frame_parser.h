#ifndef VOXMAIL_FRAME_PARSER_H
#define VOXMAIL_FRAME_PARSER_H

#include <stddef.h>

enum { VOXMAIL_FRAME_BUFFER_SIZE = 2048 };

struct voxmail_frame_parser {
	char buffer[VOXMAIL_FRAME_BUFFER_SIZE];
	size_t buffered;
};

typedef void(voxmail_frame_handler_h)(const char *frame, size_t length,
					     void *arg);

void voxmail_frame_parser_init(struct voxmail_frame_parser *parser);

/* Feed arbitrary socket chunks. A nonzero return rejects the connection. */
int voxmail_frame_parser_feed(struct voxmail_frame_parser *parser,
				      const char *data, size_t length,
				      voxmail_frame_handler_h *handler, void *arg);

#endif
