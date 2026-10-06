#include "json_output.h"

#include <stdarg.h>
#include <stdio.h>
#include <string.h>

static const char hex_digits[] = "0123456789abcdef";

bool voxmail_json_escape(char *out, size_t size, const char *src)
{
	size_t used = 0;

	if (!out || size == 0)
		return false;
	out[0] = '\0';
	if (!src)
		src = "";
	while (*src) {
		unsigned char c = (unsigned char)*src++;
		char encoded[6];
		size_t encoded_len = 1;

		switch (c) {
		case '"':
			encoded[0] = '\\';
			encoded[1] = '"';
			encoded_len = 2;
			break;
		case '\\':
			encoded[0] = '\\';
			encoded[1] = '\\';
			encoded_len = 2;
			break;
		case '\b':
			encoded[0] = '\\';
			encoded[1] = 'b';
			encoded_len = 2;
			break;
		case '\f':
			encoded[0] = '\\';
			encoded[1] = 'f';
			encoded_len = 2;
			break;
		case '\n':
			encoded[0] = '\\';
			encoded[1] = 'n';
			encoded_len = 2;
			break;
		case '\r':
			encoded[0] = '\\';
			encoded[1] = 'r';
			encoded_len = 2;
			break;
		case '\t':
			encoded[0] = '\\';
			encoded[1] = 't';
			encoded_len = 2;
			break;
		default:
			if (c < 0x20) {
				encoded[0] = '\\';
				encoded[1] = 'u';
				encoded[2] = '0';
				encoded[3] = '0';
				encoded[4] = hex_digits[c >> 4];
				encoded[5] = hex_digits[c & 0x0f];
				encoded_len = 6;
			}
			else {
				encoded[0] = (char)c;
			}
			break;
		}

		if (encoded_len >= size - used) {
			out[0] = '\0';
			return false;
		}
		memcpy(out + used, encoded, encoded_len);
		used += encoded_len;
	}
	out[used] = '\0';
	return true;
}

bool voxmail_json_append(char *out, size_t size, size_t *used,
			 const char *fmt, ...)
{
	va_list ap;
	int written;

	if (!out || !used || !fmt || *used >= size)
		return false;
	va_start(ap, fmt);
	written = vsnprintf(out + *used, size - *used, fmt, ap);
	va_end(ap);
	if (written < 0 || (size_t)written >= size - *used)
		return false;
	*used += (size_t)written;
	return true;
}
