#ifndef VOXMAIL_PCM_CLOCK_H
#define VOXMAIL_PCM_CLOCK_H
#include <stdint.h>

/* Advance on the media timeline, not callback completion time. Rebase only
 * after a long scheduling stall rather than sending a burst of stale frames. */
struct voxmail_pcm_clock {
	uint64_t next_ms;
	uint32_t ptime;
};

static inline void voxmail_pcm_clock_init(struct voxmail_pcm_clock *clock,
                                         uint64_t now, uint32_t ptime)
{
	clock->next_ms = now;
	clock->ptime = ptime ? ptime : 20;
}

static inline uint32_t voxmail_pcm_clock_wait(struct voxmail_pcm_clock *clock,
                                            uint64_t now)
{
	if (now < clock->next_ms)
		return (uint32_t)(clock->next_ms - now);
	if (now - clock->next_ms > (uint64_t)clock->ptime * 5)
		clock->next_ms = now;
	return 0;
}

static inline void voxmail_pcm_clock_advance(struct voxmail_pcm_clock *clock)
{
	clock->next_ms += clock->ptime;
}
#endif
