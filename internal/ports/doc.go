// Package ports declares the interfaces the application needs from the
// outside world.
//
// They are declared here, on the consumer side, so the core states its
// requirements and adapters satisfy them. An adapter never dictates a
// signature, and swapping one out never reaches the domain.
//
// One file per area — projects, auth, tokens, issues, config, clock — rather
// than one file with everything. That is not tidiness: every session that adds
// a repository has to touch this package, and a single file made each of them
// collide with the others in the same three lines.
package ports
