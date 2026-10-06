#include "call_dispatch.h"

#include <errno.h>

int voxmail_dispatch_call(const struct voxmail_command *command,
				  const struct voxmail_call_ops *ops, void *arg)
{
	void *call;
	int err;

	if (!command || !ops || !ops->find || !ops->release)
		return EINVAL;
	call = ops->find(arg, command->call_id);
	if (!call)
		return VOXMAIL_DISPATCH_STALE;
	if (command->type == VOXMAIL_COMMAND_ANSWER) {
		if (!ops->answer) {
			err = EINVAL;
		}
		else {
			err = ops->answer(arg, call);
		}
		ops->release(arg, call);
		return err;
	}
	if (command->type != VOXMAIL_COMMAND_HANGUP || !ops->hangup || !ops->close) {
		ops->release(arg, call);
		return EINVAL;
	}
	err = ops->hangup(arg, call,
			  command->code >= 100 && command->code <= 699 ? command->code : 603,
			  command->reason[0] ? command->reason : "Caller not authorized");
	/* A hangup is terminal from the bridge's point of view even if baresip
	 * reports an error while starting the teardown. The retained call handle
	 * is released only after the close callback has removed the session. */
	ops->close(arg, call);
	ops->release(arg, call);
	return err;
}
