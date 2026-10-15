#include <assert.h>
#include "pcm_clock.h"
#include "dtmf_guard.h"
int main(void)
{
 struct voxmail_pcm_clock clock;
 uint64_t now = 1000;
 unsigned i;
 voxmail_pcm_clock_init(&clock, now, 20);
 /* Ten minutes of callbacks taking 3 ms must not accumulate 90 seconds
  * of clock drift. Every packet still occupies its own 20 ms slot. */
 for (i = 0; i < 30000; ++i) {
  now += voxmail_pcm_clock_wait(&clock, now);
  assert(clock.next_ms == 1000 + (uint64_t)i * 20);
  now += 3;
  voxmail_pcm_clock_advance(&clock);
 }
 assert(clock.next_ms == 601000);
 now += 1000;
 assert(voxmail_pcm_clock_wait(&clock, now) == 0);
 assert(clock.next_ms == now); /* no catch-up burst after a long stall */
 struct voxmail_dtmf_guard guard = {0};
 assert(voxmail_dtmf_press(&guard, '1', 100));
 assert(!voxmail_dtmf_press(&guard, '1', 110));
 assert(voxmail_dtmf_release(&guard));
 assert(voxmail_dtmf_press(&guard, '1', 150));
 assert(voxmail_dtmf_press(&guard, '2', 170));
 assert(!voxmail_dtmf_press(&guard, '2', 180));
 /* Lost release cannot permanently swallow a later press of the same key. */
 assert(voxmail_dtmf_press(&guard, '2', 2200));
 assert(voxmail_dtmf_release(&guard));
 assert(!voxmail_dtmf_release(&guard));
 return 0;
}
