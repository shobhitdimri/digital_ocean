// Package integration holds black-box tests that run the real service: the
// production composition root (app.New and app.Run) with real file storage,
// the real LFU cache and the real logger, spoken to over real TCP.
//
// Nothing here is faked. The package contains only tests; see the unit tests
// next to each package for fault injection and the sample_test directory for a
// curl-based script that checks a deployed server.
package integration
