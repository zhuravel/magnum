// Package fixwork is the fix loop's code that both daemons need: magnum's
// engine, which runs the loop today, and the factory, which takes it over
// (DECISIONS "The fix loop moves into the factory"). It holds the JSON types
// of the fix records in magnum's registry, so both decode the same rows, and
// the loop's code that needs no engine state, its I/O passed in by the
// caller: the GitHub client (ReadCIFailures), the git client (HeldDiff), the
// read of the PR's head, the clock and its waits (ConfirmPush, which waits up
// to ConfirmWindow) and the directories its files go to (WriteCIFiles,
// CopyCIFiles, WriteHeldDiff). It imports neither internal/engine nor
// internal/factory: the factory imports the engine, so a type both need
// lives here. A name in its docs that it does not define (KVPRFix, fixJob,
// settlePushes) is the engine's.
package fixwork
