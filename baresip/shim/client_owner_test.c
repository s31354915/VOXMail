#include "client_owner.h"

#include <assert.h>
#include <errno.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

static void test_stale_generation_cannot_invalidate_reused_number(void)
{
	struct voxmail_client_owner owner = VOXMAIL_CLIENT_OWNER_INITIALIZER;
	uint64_t first_generation;
	uint64_t second_generation;

	assert(voxmail_client_owner_publish(&owner, 42, &first_generation) == -1);
	voxmail_client_owner_request_shutdown(&owner);
	assert(voxmail_client_owner_publish(&owner, 42, &second_generation) == -1);
	assert(first_generation != second_generation);
	assert(!voxmail_client_owner_invalidate_if_current(&owner, 42,
	                                                   first_generation));
	assert(voxmail_client_owner_invalidate_if_current(&owner, 42,
	                                                   second_generation));
}

static void test_snapshot_dup_and_reconnect(void)
{
	struct voxmail_client_owner owner = VOXMAIL_CLIENT_OWNER_INITIALIZER;
	int pair[2];
	int source_fd;
	int duplicate_fd;
	uint64_t generation;
	char value;

	assert(socketpair(AF_UNIX, SOCK_STREAM, 0, pair) == 0);
	assert(voxmail_client_owner_publish(&owner, pair[0], &generation) == -1);
	assert(voxmail_client_owner_snapshot_dup(&owner, &source_fd, &generation,
	                                         &duplicate_fd) == 0);
	assert(source_fd == pair[0] && duplicate_fd >= 0);
	assert(write(pair[1], "x", 1) == 1);
	assert(read(duplicate_fd, &value, 1) == 1 && value == 'x');
	close(duplicate_fd);

	voxmail_client_owner_request_shutdown(&owner);
	close(pair[0]);
	close(pair[1]);
	assert(voxmail_client_owner_snapshot_dup(&owner, &source_fd, &generation,
	                                         &duplicate_fd) == ENOTCONN);

	/* Reconnect may reuse the old integer; only the new generation is valid. */
	assert(socketpair(AF_UNIX, SOCK_STREAM, 0, pair) == 0);
	{
		uint64_t reconnected_generation;
		assert(voxmail_client_owner_publish(&owner, pair[0],
		                                    &reconnected_generation) == -1);
		assert(!voxmail_client_owner_invalidate_if_current(&owner, pair[0],
		                                                   generation));
		assert(voxmail_client_owner_invalidate_if_current(&owner, pair[0],
		                                                   reconnected_generation));
	}
	close(pair[0]);
	close(pair[1]);
}

int main(void)
{
	test_stale_generation_cannot_invalidate_reused_number();
	test_snapshot_dup_and_reconnect();
	return 0;
}
