// The CUE module identity `nix/modules/unifi-sync` assembles around a
// consumer's instance directory in the store, and the default for the
// module's `moduleFile` option.
//
// Nothing imports this module, so its name only has to be a valid one that
// does not shadow `gunk.dev/armstrong` — the instance's `import
// "gunk.dev/armstrong/schema"` has to resolve through cue.mod/pkg, which is
// where the schema derivation is linked in.
//
// The language version tracks armstrong's own cue.mod/module.cue, so the
// schema is evaluated the way it is upstream.
module: "gunk.dev/unifi-instance"
language: version: "v0.9.2"
