#ifndef VOXMAIL_DTMF_GUARD_H
#define VOXMAIL_DTMF_GUARD_H
#include <stdbool.h>
#include <stdint.h>

struct voxmail_dtmf_guard {
	char key;
	uint64_t last_ms;
};

static inline bool voxmail_dtmf_press(struct voxmail_dtmf_guard *guard,
                                    char key, uint64_t now)
{
	bool duplicate = guard->key == key && now - guard->last_ms < 2000;
	guard->key = key;
	guard->last_ms = now;
	return !duplicate;
}

static inline bool voxmail_dtmf_release(struct voxmail_dtmf_guard *guard)
{
	bool active = guard->key != 0;
	guard->key = 0;
	return active;
}
#endif
