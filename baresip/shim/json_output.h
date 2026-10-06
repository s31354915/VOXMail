#ifndef VOXMAIL_JSON_OUTPUT_H
#define VOXMAIL_JSON_OUTPUT_H

#include <stdbool.h>
#include <stddef.h>

/* Encode one UTF-8/string byte sequence for use inside a JSON string. */
bool voxmail_json_escape(char *out, size_t size, const char *src);

/* Append a bounded printf-style fragment to an existing output buffer. */
bool voxmail_json_append(char *out, size_t size, size_t *used,
			 const char *fmt, ...);

#endif
