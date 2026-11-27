// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

// Package recording records the terminal output of a run to a spool file and
// uploads it to a NOFire AI deployment, following the nofire.recording/v1 wire
// contract.
//
// The package holds no credential store and reads no environment variable. A
// caller that decided a run is recorded hands it a Client that already carries
// the access token. Whether to record is decided by the caller, not here.
//
// # What the deployment supplies is checked
//
// The deployment is trusted to decide whether to record, and to nothing else.
// A recording id must be 1 to 128 letters, digits, hyphens or underscores,
// because it goes into a URL path that carries the token. The batch size and
// the window are bounded, so they cannot ask for a large allocation. An offset
// the deployment reports must lie inside what the spool holds, and an answer
// to a batch cannot claim more than the batch brought. Any of these failing
// stops the upload. A text the deployment sends is never printed.
//
// # A quiet run stays recorded
//
// The deployment closes a recording that hears nothing for 2 minutes. While a
// recording is open, the uploader sends an empty batch at the acknowledged
// offset when 45 seconds have passed since its last good request. The empty
// batch stores nothing. It says the recorder is alive. It follows the same
// rules as an append for 429, 5xx, transport errors and 401 or 403.
//
// # A recorder that was cut off continues in a new recording
//
// A recorder can be cut off for more than 2 minutes: the machine slept or the
// network was down. The deployment then closes the recording, and an append or
// a keepalive answers 409 recording_closed. The recorder opens a new recording
// under a new run key and the terminal's current size. The new recording
// starts at offset 0 with no dictionary.
//
// The spool holds the run's whole frame stream. A recording's raw stream is
// its Prefix followed by the spool from its Base on. The Prefix of a recording
// that continues another is one resize frame with the current size. The Base is
// the spool position of the first frame the earlier recording did not take. So
// recording offset N is spool position Base + N - len(Prefix). The Uploader
// does all its reading through this mapping, and the meta file keeps Prefix and
// SpoolBase so that `brig recordings flush` can finish the run after a crash.
// A run may use any number of recordings. A loop guard limits only how often a
// continuation is opened. The first continuation opens at once. If the server
// closes a continuation less than 60 seconds after it opened, the next open
// waits 60 seconds. Each further early closure doubles the wait, up to 5
// minutes. A continuation that stays open for 10 minutes clears the wait. The
// spool keeps growing during a wait. If the run ends during a wait, the spool
// stays for `brig recordings flush`, which follows the same guard.
//
// The open of a new recording is retried while the run lives, after a
// transport error, a timeout, 429 or 5xx, with the back-off of an append. The
// spool keeps growing meanwhile. If the run ends before an open succeeds, the
// spool stays for `brig recordings flush`, which continues in a new recording.
// Only an answer of recorded false stops recording for the rest of the run and
// removes the spool, as for a run that was never recorded. A refused login
// keeps the spool and stops uploading. The first open of a run is different:
// any failure of it means the run is not recorded.
//
// # The end of a run
//
// When the run ends, the session catches up and closes the recording, but waits
// for the deployment at most Params.DrainTimeout, 5 seconds by default. If the
// deployment did not take everything or did not confirm the close, the spool
// and its meta file stay on the machine, on the first recording as much as on
// a continuation.
//
// A SIGINT, SIGTERM or SIGHUP that reaches Brig after the command has ended, also
// while the relay is still waiting for the last of its output, ends that wait
// and leaves the spool for a later flush. One that came while the command ran
// was forwarded to it by the relay and is dropped when the command ends.
//
// Whatever the end of the run or a flush ended with, the position the deployment
// holds is saved in the meta file, so a later continuation in a new recording
// starts there and no output is sent twice.
//
// # What earlier runs left
//
// A recorded run starts one background flush after its own open answered
// recorded true. It sends the spools earlier runs left in the same directory,
// with the rules of `brig recordings flush`: the same loop guard, and a refused
// login ends it quietly. It never delays the start of the run, prints nothing
// while the run goes on, and is cancelled when the run ends, which leaves each
// spool as resumable as before. The end of the run waits 2 seconds for it to
// stop. A flush still stuck then, in a slow keychain read for instance, goes on
// in the background, and the run reports the recordings it had finished by then.
// It skips this run's own spool and any spool a
// live recorder holds, which it tells by the shared lock that a live recorder's
// exclusive lock refuses. A run keeps that lock until it has removed its spool or
// decided to leave it, so no other process sends a spool that is still draining.
//
// The login stored now need not be the one that recorded a spool. A flush never
// begins with an open request. It first sends an empty keepalive for the
// recording id in the meta file, with the current token. A 200 whose body is not
// JSON is a proxy's or a captive portal's page and is no answer: the batch,
// keepalive and close calls wait and send again, and the check leaves the spool
// for a later flush. A 200 or a 409
// offset_mismatch says the recording is this login's and open: the flush sends
// from the deployment's offset. A 409 recording_closed says it is this login's
// and ended: only then does the flush continue the run in a new recording. A 404
// says it is not this login's: nothing is sent, nothing is opened and the spool
// stays. A spool is removed once it is older than MaxSpoolAge only for a reason a
// later sign-in does not fix: the 404, a meta file that cannot be used, or an offset
// the deployment names outside the spool. A
// spool that only has no usable login now stays however old it is. A run removes
// its meta file before its spool, so a meta file whose spool is gone is not a
// recording being tidied: the flush removes it and counts it as removed without
// being sent, and a pair that was removed meanwhile is not counted. A run that
// continues in a new recording uses the token it started with.
//
// Only one flush runs at a time on a machine: a flush
// takes an exclusive lock on the spool directory without waiting and sends
// nothing when another holds it.
//
// # A relay that cannot start
//
// A failure before the command has started (no pseudo-terminal, no pipe, or the
// command cannot start, all reported as ptyrelay.ErrNotStarted) leaves nothing
// run and the person's terminal untouched. The session closes the recording it
// opened with the end reason recorder_error, removes its files and says the run
// is not recorded after all, and the caller runs the command without the relay.
// A failure after the command ran cannot be undone: it is returned as it was.
//
// # What the user is told
//
// Nothing is printed while the run goes on. When it ends, the session prints
// to Params.Notice one line for each thing that happened: that the recording was
// interrupted and continues in another, that the end of the run's recording is
// still on this machine and will be sent by the next recorded run or by `brig
// recordings flush`, that recording stopped and from what time the output is not
// recorded (which a deployment that refuses the recording while the run goes on
// causes too: the spool is removed at once and nothing more is written; a spool
// Brig cannot send, with an invalid frame or a frame larger than the
// deployment's batch size, ends the recording the same way and the line says
// so), that
// the spool cap or a write error ended the recording part way through the run,
// that the deployment refused the recording as the run ended and the rest was
// not sent, that the deployment refused the login and `brig login` is needed
// (in place of the line about the next recorded run), that the background flush
// sent the rest of n earlier recordings, or that it removed n earlier
// recordings without sending them.
//
// # A deployment that does not let the upload move
//
// A 409 offset_mismatch that names an offset other than the uploader's own is
// ordinary: the uploader takes the offset it names and sends from there at
// once. One that names the offset the batch was sent at means the deployment is
// not moving, and so does a third mismatch with no batch accepted in between,
// whatever offset it names, as when two replicas hold different offsets. The
// uploader then takes the offset and waits as it does after a transport error,
// 1 second doubling to 30, and keeps the spool. An accepted batch, or a switch
// to a new recording, resets the count.
package recording
