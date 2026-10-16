// Package sandbox runs untrusted-input tooling (git, helm, kustomize, the jsonnet
// evaluator, config management plugins) and handles untrusted file trees for the
// repo-server and CMP sidecars, as decided in ADR-0007.
//
// Subprocess execution ([Run]):
//   - the command environment is exactly [Cmd.Env]; nothing is inherited from the
//     calling process ([BaseEnv] builds a minimal allow-listed environment);
//   - the child runs in its own process group, and the whole group is killed on
//     timeout, context cancellation, output overflow and after the leader exits;
//   - stdout and stderr are captured with hard caps and drained without deadlock even
//     when grandchildren keep the pipes open;
//   - only stdin, stdout and stderr are passed to the child: Run marks every
//     descriptor of the process close-on-exec before a direct start, and the
//     helper does the same before exec;
//   - CPU-time, address-space and open-file limits are applied by re-executing the
//     current binary as a tiny helper ([MaybeHelperMain]) that calls setrlimit,
//     acknowledges over a status pipe and then execs the target in place; a binary
//     that does not run the helper makes Run fail with ErrInvalidCommand.
//
// File system safety ([SecureJoin], [CheckSymlinksContext], [ExtractTarContext],
// [ExtractTarGzContext], [TarCountContext] and their context-free forms) keeps
// every path, symlink and archive entry inside a root directory, with work bounded
// per entry and per walk so untrusted trees and archives cannot pin the CPU.
//
// The package does not import api/v1alpha1. Callers adapt CRD fields (for example
// helm/kustomize settings or repo-server limits from settings ConfigMaps) into
// [Limits] and [ArchiveLimits].
package sandbox
