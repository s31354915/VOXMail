#include "frame_parser.h"

#include <errno.h>
#include <stdbool.h>

void voxmail_frame_parser_init(struct voxmail_frame_parser *parser)
{
	if (!parser)
		return;
	parser->buffered = 0;
	parser->buffer[0] = '\0';
}

int voxmail_frame_parser_feed(struct voxmail_frame_parser *parser,
				      const char *data, size_t length,
				      voxmail_frame_handler_h *handler, void *arg)
{
	size_t i;

	if (!parser || (!data && length > 0) || !handler)
		return EINVAL;
	for (i = 0; i < length; ++i) {
		unsigned char c = (unsigned char)data[i];
		if (c == '\0') {
			parser->buffered = 0;
			parser->buffer[0] = '\0';
			return EPROTO;
		}
		if (c == '\n') {
			if (parser->buffered > 0) {
				parser->buffer[parser->buffered] = '\0';
				handler(parser->buffer, parser->buffered, arg);
				parser->buffered = 0;
			}
			continue;
		}
		if (parser->buffered + 1 >= sizeof(parser->buffer)) {
			parser->buffered = 0;
			parser->buffer[0] = '\0';
			return EMSGSIZE;
		}
		parser->buffer[parser->buffered++] = (char)c;
	}
	return 0;
}
