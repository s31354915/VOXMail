#ifndef VOXMAIL_CALL_DISPATCH_H
#define VOXMAIL_CALL_DISPATCH_H

#include "command_queue.h"

enum voxmail_dispatch_result {
	VOXMAIL_DISPATCH_EXECUTED = 0,
	VOXMAIL_DISPATCH_STALE = 2,
};

struct voxmail_call_ops {
	void *(*find)(void *arg, const char *call_id);
	int (*answer)(void *arg, void *call);
	int (*hangup)(void *arg, void *call, int code, const char *reason);
	void (*close)(void *arg, void *call);
	void (*release)(void *arg, void *call);
};

int voxmail_dispatch_call(const struct voxmail_command *command,
				  const struct voxmail_call_ops *ops, void *arg);

#endif
