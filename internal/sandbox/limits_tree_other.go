//go:build !linux

package sandbox

// treeWatchable: off Linux the tree limits have another killer — the Windows
// job reports its memory and process limits on its completion port
// (start_windows.go) — or none (macOS: the sh ulimits only), so the limit
// watch samples only the scratch size there.
const treeWatchable = false

func sampleTree(int) treeSample { return treeSample{} }
