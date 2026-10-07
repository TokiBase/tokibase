package cmd

// AnnotationSkipBootstrap marks commands that must run without bootstrapping
// the app (they work on a replica or on a data dir that does not exist yet).
const AnnotationSkipBootstrap = "tokibase/skipBootstrap"
