// Package fixwork is the fix loop's code that both daemons need: magnum's
// engine, which runs the loop today, and the factory, which takes it over
// (DECISIONS "The fix loop moves into the factory"). It holds the JSON types
// of the fix records in magnum's registry, so both decode the same rows, and
// the pure parts of the loop. It imports neither internal/engine nor
// internal/factory: the factory imports the engine, so a type both need
// lives here. A name in its docs that it does not define (KVPRFix, fixJob,
// settlePushes) is the engine's.
package fixwork
