#include "json_output.h"

#include <assert.h>
#include <string.h>

static void test_escaping(void)
{
	char out[128];
	const char input[] = "quote\" slash\\ controls\b\f\n\r\t\x01";

	assert(voxmail_json_escape(out, sizeof(out), input));
	assert(!strcmp(out,
	               "quote\\\" slash\\\\ controls\\b\\f\\n\\r\\t\\u0001"));
	assert(voxmail_json_escape(out, sizeof(out), NULL));
	assert(!strcmp(out, ""));
}

static void test_escape_fails_closed(void)
{
	char out[8];
	char control_out[6];

	assert(!voxmail_json_escape(out, sizeof(out), "12345678"));
	assert(out[0] == '\0');
	assert(!voxmail_json_escape(control_out, sizeof(control_out), "\x01"));
	assert(control_out[0] == '\0');
	assert(!voxmail_json_escape(NULL, sizeof(out), "x"));
	assert(!voxmail_json_escape(out, 0, "x"));
}

static void test_bounded_append(void)
{
	char out[16] = "prefix";
	size_t used = strlen(out);

	assert(voxmail_json_append(out, sizeof(out), &used, ":%s", "ok"));
	assert(!strcmp(out, "prefix:ok"));
	assert(used == strlen(out));
	assert(!voxmail_json_append(out, sizeof(out), &used, "%s", "0123456789"));
	used = sizeof(out);
	assert(!voxmail_json_append(out, sizeof(out), &used, "x"));
}

int main(void)
{
	test_escaping();
	test_escape_fails_closed();
	test_bounded_append();
	return 0;
}
