#!/bin/sh
# Test-only Piper stand-in. It accepts the real Piper argument shape and
# stops itself so context cancellation must terminate the exact child
# process; it intentionally never writes an output file.
kill -STOP $$
