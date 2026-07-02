package storage

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash/fnv"
)

// ComputeSourceHash computes the SourceHash for an agent as:
//
//	hex(sha256(manifestVersion + hex(sha256(fileContent))))
//
// This means a change to either the manifest version string or the file
// content produces a distinct hash, satisfying the SourceHash versioning
// contract defined in CONTEXT.md.
func ComputeSourceHash(manifestVersion string, fileContent []byte) string {
	contentHashBytes := sha256.Sum256(fileContent)
	fileContentHash := hex.EncodeToString(contentHashBytes[:])

	combined := manifestVersion + fileContentHash
	finalHashBytes := sha256.Sum256([]byte(combined))
	return hex.EncodeToString(finalHashBytes[:])
}

// ComputeSidecarSourceHash computes the SourceHash for a sidecar Tool-Agent as:
//
//	hex(FNV-1a(version_bytes + manifest_json_bytes + binary_file_bytes))
//
// A change to either the manifest JSON or the binary file produces a distinct
// hash and triggers re-registration, satisfying the sidecar SourceHash contract
// defined in issue #0008-03.
func ComputeSidecarSourceHash(version string, manifestJSON []byte, binaryBytes []byte) string {
	h := fnv.New64a()
	// Prefix each segment with its length to prevent boundary collisions
	// e.g. version="ab"+manifest="cd" ≠ version="a"+manifest="bcd"
	lenBuf := make([]byte, 8)

	binary.LittleEndian.PutUint64(lenBuf, uint64(len(version)))
	h.Write(lenBuf)
	h.Write([]byte(version))

	binary.LittleEndian.PutUint64(lenBuf, uint64(len(manifestJSON)))
	h.Write(lenBuf)
	h.Write(manifestJSON)

	binary.LittleEndian.PutUint64(lenBuf, uint64(len(binaryBytes)))
	h.Write(lenBuf)
	h.Write(binaryBytes)

	sum := h.Sum64()
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, sum)
	return hex.EncodeToString(buf)
}
