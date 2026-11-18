package backup

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Index builds during a restore (ENG-2123).
//
// pg_restore rebuilds every index in one go, and pgvector builds an HNSW index with
// parallel maintenance workers that share the graph through a dynamic shared memory
// segment of about maintenance_work_mem, allocated in /dev/shm. Docker gives a container
// 64 MB of /dev/shm unless shm_size is set, so on the shipped container the parallel
// build of idx_chunks_embedding_cosine fails ("could not resize shared memory segment …
// No space left on device") once the store holds a few thousand chunks — exactly when a
// restore is needed. The live database never hits this: its index grows row by row.
//
// So before a restore the session plan checks the free shared memory and, when it is too
// small or cannot be measured, runs the restore's session with
// max_parallel_maintenance_workers=0 (PGOPTIONS, so only that pg_restore session changes;
// no server setting is touched). A serial build needs no shared memory segment and is
// correct on any server, only slower. Rejected: failing early with a message — the
// operator would then have to restart the database container mid-incident; the restore
// should just work on the shipped database (owner decision 2026-11-18).

// parallelShmFactor is how many maintenance_work_mem's worth of free /dev/shm a parallel
// index build needs before the restore allows it: one for pgvector's shared graph
// segment, one of headroom for the parallel heap scan and other dynamic shared memory
// users on the same server.
const parallelShmFactor = 2

// SerialIndexBuildOptions is the PGOPTIONS value that turns off parallel index builds for
// one session.
const SerialIndexBuildOptions = "-c max_parallel_maintenance_workers=0"

// IndexBuildPlan is how a restore session builds its indexes and why.
type IndexBuildPlan struct {
	// SessionOptions is the PGOPTIONS value for the pg_restore session ("" = server defaults).
	SessionOptions string
	// Reason says what was measured, for the operator log.
	Reason string
}

// Serial reports whether the plan turns off parallel index builds.
func (p IndexBuildPlan) Serial() bool { return p.SessionOptions != "" }

// decideIndexBuild is the pure rule: shmFree < 0 means unknown.
func decideIndexBuild(shmFree, maintWorkMem int64, maxWorkers int) IndexBuildPlan {
	switch {
	case maxWorkers <= 0:
		return IndexBuildPlan{Reason: "the server's max_parallel_maintenance_workers is 0; index builds are serial already"}
	case shmFree < 0:
		return IndexBuildPlan{SessionOptions: SerialIndexBuildOptions, Reason: "free shared memory (/dev/shm) on the " +
			"database server cannot be measured (set backup.pg_container to measure it); building indexes serially " +
			"in the restore session so a parallel HNSW build cannot run out of shared memory"}
	case shmFree < parallelShmFactor*maintWorkMem:
		return IndexBuildPlan{SessionOptions: SerialIndexBuildOptions, Reason: fmt.Sprintf(
			"the database container has %s free in /dev/shm, under the %s a parallel index build needs "+
				"(%d x maintenance_work_mem %s); building indexes serially in the restore session. "+
				"To restore faster, give the container more shared memory (docker compose: shm_size: 1g; "+
				"docker run: --shm-size=1g) and recreate it",
			mib(shmFree), mib(parallelShmFactor*maintWorkMem), parallelShmFactor, mib(maintWorkMem))}
	default:
		return IndexBuildPlan{Reason: fmt.Sprintf("the database container has %s free in /dev/shm (needs %s); "+
			"parallel index builds allowed", mib(shmFree), mib(parallelShmFactor*maintWorkMem))}
	}
}

func mib(b int64) string { return fmt.Sprintf("%d MiB", b/(1<<20)) }

// PlanIndexBuild measures the server for the restore's index builds: maintenance_work_mem
// and max_parallel_maintenance_workers from the server, free /dev/shm from inside
// backup.pg_container (unknown without a container, which plans a serial build).
func (p PG) PlanIndexBuild(ctx context.Context) (IndexBuildPlan, error) {
	conn, err := p.connect(ctx, "postgres")
	if err != nil {
		return IndexBuildPlan{}, err
	}
	defer conn.Close(ctx)
	var mwm int64
	var workers int
	if err := conn.QueryRow(ctx, `SELECT pg_size_bytes(current_setting('maintenance_work_mem')),
		current_setting('max_parallel_maintenance_workers')::int`).Scan(&mwm, &workers); err != nil {
		return IndexBuildPlan{}, fmt.Errorf("read index build settings: %w", err)
	}
	return decideIndexBuild(p.freeShm(ctx), mwm, workers), nil
}

// freeShm is the free space of /dev/shm inside backup.pg_container in bytes, or -1 when
// there is no container or it cannot be read.
func (p PG) freeShm(ctx context.Context) int64 {
	if p.Container == "" {
		return -1
	}
	cmd, err := p.dockerExec(ctx, "df", "-kP", "/dev/shm")
	if err != nil {
		return -1
	}
	out, err := cmd.Output()
	if err != nil {
		return -1
	}
	n, ok := parseDfAvailKB(string(out))
	if !ok {
		return -1
	}
	return n * 1024
}

// parseDfAvailKB reads the "Available" column of `df -kP` output (POSIX format: header,
// then one line per filesystem: name, blocks, used, available, capacity, mount).
func parseDfAvailKB(out string) (int64, bool) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, false
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 6 {
		return 0, false
	}
	n, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
