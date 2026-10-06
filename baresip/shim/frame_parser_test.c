#include "frame_parser.h"

#include <assert.h>
#include <errno.h>
#include <stdint.h>
#include <string.h>

struct collected_frames {
	char frames[4][VOXMAIL_FRAME_BUFFER_SIZE];
	size_t lengths[4];
	size_t count;
};

static void collect_frame(const char *frame, size_t length, void *arg)
{
	struct collected_frames *collected = arg;
	assert(collected->count < 4);
	assert(length < sizeof(collected->frames[0]));
	memcpy(collected->frames[collected->count], frame, length + 1);
	collected->lengths[collected->count++] = length;
}

static void count_frame(const char *frame, size_t length, void *arg)
{
	size_t *count = arg;
	(void)frame;
	(void)length;
	++*count;
}

static void test_split_and_coalesced_frames(void)
{
	struct voxmail_frame_parser parser;
	struct collected_frames collected = {0};

	voxmail_frame_parser_init(&parser);
	assert(voxmail_frame_parser_feed(&parser, "{\"type\":\"di",
	                                 strlen("{\"type\":\"di"),
	                                 collect_frame, &collected) == 0);
	assert(collected.count == 0);
	assert(voxmail_frame_parser_feed(&parser, "al\"}\n",
	                                 strlen("al\"}\n"),
	                                 collect_frame, &collected) == 0);
	assert(collected.count == 1);
	assert(!strcmp(collected.frames[0], "{\"type\":\"dial\"}"));
	assert(collected.lengths[0] == strlen(collected.frames[0]));

	assert(voxmail_frame_parser_feed(&parser,
	                                 "{\"x\":1}\n{\"x\":2}\n",
	                                 strlen("{\"x\":1}\n{\"x\":2}\n"),
	                                 collect_frame, &collected) == 0);
	assert(collected.count == 3);
	assert(!strcmp(collected.frames[1], "{\"x\":1}"));
	assert(!strcmp(collected.frames[2], "{\"x\":2}"));
}

static void test_escape_and_empty_lines(void)
{
	struct voxmail_frame_parser parser;
	struct collected_frames collected = {0};
	const char *input = "\n{\"value\":\"quote\\\"\\\\\"}\n";

	voxmail_frame_parser_init(&parser);
	assert(voxmail_frame_parser_feed(&parser, input, strlen(input),
	                                 collect_frame, &collected) == 0);
	assert(collected.count == 1);
	assert(!strcmp(collected.frames[0], "{\"value\":\"quote\\\"\\\\\"}"));
}

static void test_oversized_and_nul_frames_fail_closed(void)
{
	struct voxmail_frame_parser parser;
	struct collected_frames collected = {0};
	char oversized[VOXMAIL_FRAME_BUFFER_SIZE];

	memset(oversized, 'a', sizeof(oversized));
	voxmail_frame_parser_init(&parser);
	assert(voxmail_frame_parser_feed(&parser, oversized, sizeof(oversized),
	                                 collect_frame, &collected) == EMSGSIZE);
	assert(collected.count == 0 && parser.buffered == 0);
	assert(voxmail_frame_parser_feed(&parser, "ok\0\n", 5,
	                                 collect_frame, &collected) == EPROTO);
	assert(collected.count == 0 && parser.buffered == 0);

	/* A new accepted descriptor gets a fresh parser state after rejection. */
	voxmail_frame_parser_init(&parser);
	assert(voxmail_frame_parser_feed(&parser, "reconnected\n", 12,
	                                 collect_frame, &collected) == 0);
	assert(collected.count == 1);
	assert(!strcmp(collected.frames[0], "reconnected"));
}

static uint32_t next_random(uint32_t *state)
{
	*state = *state * 1664525u + 1013904223u;
	return *state;
}

static void test_randomized_frame_corpus(void)
{
	struct voxmail_frame_parser parser;
	char input[4096];
	uint32_t state = 0xC0FFEEu;

	for (unsigned iteration = 0; iteration < 10000; ++iteration) {
		size_t length = next_random(&state) % sizeof(input);
		size_t count = 0;
		for (size_t i = 0; i < length; ++i)
			input[i] = (char)next_random(&state);
		voxmail_frame_parser_init(&parser);
		int err = voxmail_frame_parser_feed(&parser, input, length,
		                                  count_frame, &count);
		assert(err == 0 || err == EPROTO || err == EMSGSIZE);
		if (err != 0)
			assert(parser.buffered == 0);
	}
}

int main(void)
{
	test_split_and_coalesced_frames();
	test_escape_and_empty_lines();
	test_oversized_and_nul_frames_fail_closed();
	test_randomized_frame_corpus();
	return 0;
}
