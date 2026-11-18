// armstrong's project layer: what this one project declares above the
// floor. The floor is the owning valley's policy/instance; this layer binds
// armstrong alone, and a project can only add to its own layer.
package verification

project: {
	// Every check below is the flake check of the same name in flake.nix,
	// built for the runner's own system.
	let ownChecks = [
		"cue-vet",
		"dns",
		"go-test",
		"go-vet",
		"nixfmt",
		"schema",
		"schema-consumer",
		"unifi",
		"unifi-sync-eval",
		"unifi-sync-vm",
		"yamllint",
		"zizmor",
	]
	for n in ownChecks {
		checks: (n): {
			runner:    "nix"
			attribute: n
		}
	}

	// unifi-sync-vm boots a NixOS machine and needs KVM. Every other check
	// takes seconds once the Go modules are fetched.
	checks: "unifi-sync-vm": cost: "slow"

	// The Go CLIs, the CUE schema and its examples, and the NixOS module, as
	// one class, because the checks read across these parts: the unifi tests
	// shell out to cue against schema/, and unifi-sync-vm runs the unifi
	// binary and the schema through the module's own unit. The workflow
	// linters read only .github/.
	classes: source: {
		paths: "cmd/**":      true
		paths: "go.mod":      true
		paths: "go.sum":      true
		paths: "schema/**":   true
		paths: "cue.mod/**":  true
		paths: "examples/**": true
		paths: "nix/**":      true
		requires: {for n in ownChecks if n != "yamllint" && n != "zizmor" {(n): true}}
	}

	classes: workflows: {
		paths: ".github/**": true
		requires: yamllint:  true
		requires: zizmor:    true
	}

	// flake.nix defines every check, flake.lock pins the nixpkgs each is
	// built from, and this policy decides which of them a change owes. An
	// approval of a change here carries across no landing.
	classes: "flake-and-policy": {
		paths: "flake.nix":  true
		paths: "flake.lock": true
		paths: "policy/**":  true
		requires: {for n in ownChecks {(n): true}}
		carry: exact: true
	}

	// A path no class names is one the policy has fallen behind, such as a
	// new Go package that go-test and go-vet reach through ./..., or a Nix
	// file nixfmt finds anywhere in the tree. It requires every check, as
	// do .gitignore and the editor settings under .claude/.
	unclassified: {for n in ownChecks {(n): true}}
}
