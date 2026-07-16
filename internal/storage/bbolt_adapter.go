package storage

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

// sidecarManifest is the JSON shape of a *.manifest.json sidecar file.
type sidecarManifest struct {
	Trait            string   `json:"trait"`
	Version          string   `json:"version"`
	ExecPath         string   `json:"exec_path"`
	Description      string   `json:"description"`
	SupportedFormats []string `json:"supported_formats"`
	Runtime          string   `json:"runtime"`
}

var agentBucket = []byte("agents")
var manifestBucket = []byte("manifests")
var taskEventBucket = []byte("task_events")
var checkpointBucket = []byte("checkpoints")
var sessionBucket = []byte("sessions")
var eventBucket = []byte("events")
var artifactBucket = []byte("artifacts")
var clusterBucket = []byte("capability_clusters")
var watchConfigBucket = []byte("watch_configs")
var planEventBucket = []byte("plan_events")
var retrievalSessionBucket = []byte("retrieval_sessions")
var traversalLogBucket = []byte("traversal_log")
var contradictionResolutionBucket = []byte("contradiction_resolutions")
var descRegex = regexp.MustCompile(`AGENT_DESCRIPTION\s*=\s*(?:"|')([^"']+)(?:"|')`)
var manifestRegex = regexp.MustCompile(`(?s)AGENT_MANIFEST\s*=\s*'''([\s\S]*?)'''`)

type BBoltAdapter struct {
	db          *bbolt.DB
	CardFetcher A2ACardFetcher // for A2A agent registration; nil means RegisterA2AAgent disabled
	Enqueuer    Enqueuer       // for Interview enqueue after registration
}

func NewBBoltAdapter(dbPath string, agentsDir string, isSystemAgent func(id string) bool) (*BBoltAdapter, error) {
	db, err := bbolt.Open(dbPath, 0600, &bbolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("bbolt database open failed: %w", err)
	}

	adapter := &BBoltAdapter{db: db}

	// Create buckets and run initial seed
	if err := adapter.Seed(agentsDir, isSystemAgent); err != nil {
		db.Close()
		return nil, fmt.Errorf("bbolt seed failed: %w", err)
	}

	return adapter, nil
}

// Seed creates buckets if they don't exist and populates the database by scanning
// agentsDir recursively. Three shapes are supported: single-file `*_agent.py`,
// a Python package (directory with `__init__.py` + `agent.py`), and `*.manifest.json`
// sidecars. The recursive scan reaches nested subdirectories such as agents/system/,
// which is required for the privileged system organs. isSystemAgent (nil-safe)
// stamps AgentRecord.System at registration; nil ⇒ no agent is classified. Safe
// to call multiple times — existing agents are only updated when SourceHash changes.
//
// Separated from NewBBoltAdapter so callers that only need a DB handle (e.g. tests)
// can construct without running a full filesystem scan.
func (b *BBoltAdapter) Seed(agentsDir string, isSystemAgent func(id string) bool) error {
	systemID := isSystemAgent
	if systemID == nil {
		systemID = func(string) bool { return false }
	}

	if err := b.db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(agentBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(manifestBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucketIfNotExists(taskEventBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucketIfNotExists(checkpointBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucketIfNotExists(sessionBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucketIfNotExists(eventBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucketIfNotExists(artifactBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucketIfNotExists(clusterBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucketIfNotExists(planEventBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(retrievalSessionBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(traversalLogBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(contradictionResolutionBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucketIfNotExists(watchConfigBucket); err != nil {
			return err
		}

		// REACT-01 (ADR-0061): durable reactive-execution buckets.
		if _, err := tx.CreateBucketIfNotExists(reactiveJournalBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(reactiveCursorBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(reactiveIdempotencyBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists(reactiveDeadLetterBucket); err != nil {
			return err
		}

		walkErr := filepath.WalkDir(agentsDir, func(walkPath string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				slog.Warn("DB (BBOLT): agents walk error, skipping", "path", walkPath, "err", walkErr)
				return nil
			}
			if d.IsDir() {
				if walkPath == agentsDir {
					return nil
				}
				if isAgentPackage(walkPath) {
					id := d.Name()
					entry := filepath.Join(walkPath, "agent.py")
					if err := seedPythonAgent(tx, agentsDir, entry, id, systemID); err != nil {
						return err
					}
					return fs.SkipDir
				}
				return nil
			}
			name := d.Name()
			switch {
			case strings.HasSuffix(name, "agent.py"):
				id := strings.TrimSuffix(name, ".py")
				return seedPythonAgent(tx, agentsDir, walkPath, id, systemID)
			case strings.HasSuffix(name, ".manifest.json"):
				id := strings.TrimSuffix(name, ".manifest.json")
				return seedSidecarAgent(tx, agentsDir, walkPath, id, systemID)
			}
			return nil
		})
		if walkErr != nil {
			slog.Warn("DB (BBOLT): agents dir unreadable, skipping seed", "dir", agentsDir, "err", walkErr)
		}

		return nil
	}); err != nil {
		return err
	}

	return b.checkSystemAgentLayout(agentsDir)
}

// isAgentPackage reports whether dir is a Python package that also ships the
// agent entry point. Detected by the presence of both `__init__.py` (the Python
// package marker) and `agent.py` (the seeder's convention for the gRPC entry).
func isAgentPackage(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "__init__.py")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "agent.py")); err != nil {
		return false
	}
	return true
}

// checkSystemAgentLayout warns when a System=true agent lives outside the
// <agentsDir>/system/ subtree. The file is honored; the warning is a soft
// signal that the package-layout convention is broken.
func (b *BBoltAdapter) checkSystemAgentLayout(agentsDir string) error {
	systemDir := filepath.ToSlash(filepath.Join(agentsDir, "system"))
	systemDirPrefix := systemDir + "/"
	return b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(agentBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, v []byte) error {
			var rec AgentRecord
			if json.Unmarshal(v, &rec) != nil {
				return nil
			}
			if rec.System && !strings.HasPrefix(rec.ExecPath, systemDirPrefix) {
				slog.Warn("DB (BBOLT): system agent registered outside agents/system/ subtree — file honored, convention broken",
					"id", rec.ID, "exec_path", rec.ExecPath, "system_dir", systemDir)
			}
			return nil
		})
	})
}

func seedPythonAgent(tx *bbolt.Tx, agentsDir, fullPath, id string, isSystemAgent func(string) bool) error {
	agentsBucket := tx.Bucket(agentBucket)
	manifestsBucket := tx.Bucket(manifestBucket)
	if agentsBucket == nil || manifestsBucket == nil {
		return fmt.Errorf("seedPythonAgent: required buckets missing")
	}

	content, readErr := os.ReadFile(fullPath)

	description := "General-purpose agent."
	if readErr == nil {
		matches := descRegex.FindSubmatch(content)
		if len(matches) > 1 {
			description = string(matches[1])
		}
	}

	var manifest ManifestRecord
	if readErr == nil {
		mMatches := manifestRegex.FindSubmatch(content)
		if len(mMatches) > 1 {
			if jsonErr := json.Unmarshal(mMatches[1], &manifest); jsonErr != nil {
				slog.Warn("DB (BBOLT): agent manifest JSON parse error", "id", id, "err", jsonErr)
				manifest = ManifestRecord{}
			}
		}
	}

	var fileContent []byte
	if readErr == nil {
		fileContent = content
	}
	sourceHash := ComputeSourceHash(manifest.Version, fileContent)

	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshal manifest for %s: %w", id, err)
	}
	if err := manifestsBucket.Put([]byte(id), manifestData); err != nil {
		return fmt.Errorf("put manifest for %s: %w", id, err)
	}

	execPath := filepath.ToSlash(fullPath)
	// ExecPath must be relative to Dir — buildAgentCmd resolves it under cmd.Dir=def.Dir,
	// so a full path produces a doubled 'agents/agents/...' and python can't open the file.
	if rel, relErr := filepath.Rel(agentsDir, fullPath); relErr == nil && !strings.HasPrefix(rel, "..") {
		execPath = filepath.ToSlash(rel)
	}

	existingData := agentsBucket.Get([]byte(id))
	if existingData == nil {
		slog.Info("DB (BBOLT): seeding new agent...", "id", id)
		agent := AgentRecord{
			ID:              id,
			Name:            strings.ReplaceAll(strings.Title(strings.ReplaceAll(id, "_", " ")), " Agent", " Agent"),
			Description:     description,
			Runtime:         "python",
			ExecPath:        execPath,
			Dir:             filepath.ToSlash(agentsDir),
			SourceHash:      sourceHash,
			ManifestVersion: manifest.Version,
			Provisional:     true,
			Trait:           manifest.Trait,
			System:          isSystemAgent(id),
		}
		data, err := json.Marshal(agent)
		if err != nil {
			return fmt.Errorf("marshal new sidecar agent %s: %w", id, err)
		}
		if err := agentsBucket.Put([]byte(id), data); err != nil {
			return fmt.Errorf("put new sidecar agent %s: %w", id, err)
		}
	} else {
		var existing AgentRecord
		if jsonErr := json.Unmarshal(existingData, &existing); jsonErr == nil {
			if existing.SourceHash != sourceHash {
				slog.Info("DB (BBOLT): agent source changed, marking Provisional", "id", id)
				existing.SourceHash = sourceHash
				existing.ManifestVersion = manifest.Version
				existing.Provisional = true
				existing.Description = description
				existing.System = isSystemAgent(id)
				data, err := json.Marshal(existing)
				if err != nil {
					return fmt.Errorf("marshal updated sidecar agent %s: %w", id, err)
				}
				if err := agentsBucket.Put([]byte(id), data); err != nil {
					return fmt.Errorf("put updated sidecar agent %s: %w", id, err)
				}
			}
		}
	}
	return nil
}

func seedSidecarAgent(tx *bbolt.Tx, agentsDir, manifestPath, id string, isSystemAgent func(string) bool) error {
	agentsBucket := tx.Bucket(agentBucket)
	manifestsBucket := tx.Bucket(manifestBucket)
	if agentsBucket == nil || manifestsBucket == nil {
		return fmt.Errorf("seedSidecarAgent: required buckets missing")
	}

	manifestBytes, readErr := os.ReadFile(manifestPath)
	if readErr != nil {
		slog.Warn("DB (BBOLT): sidecar manifest unreadable, skipping",
			"file", manifestPath, "err", readErr)
		return nil
	}

	var sm sidecarManifest
	if jsonErr := json.Unmarshal(manifestBytes, &sm); jsonErr != nil {
		slog.Warn("DB (BBOLT): sidecar manifest JSON parse error, skipping",
			"file", manifestPath, "err", jsonErr)
		return nil
	}

	// trait field must be "tool" — anything else (absent, "cognitive", etc.) is skipped.
	if sm.Trait != "tool" {
		slog.Warn("DB (BBOLT): sidecar manifest trait is not 'tool', skipping",
			"file", manifestPath, "trait", sm.Trait)
		return nil
	}

	// Resolve exec_path relative to the manifest's own directory.
	manifestDir := filepath.Dir(manifestPath)
	resolvedExecPath := filepath.ToSlash(
		filepath.Join(manifestDir, filepath.FromSlash(sm.ExecPath)))

	// Binary must exist; if not, log WARN and skip (do not crash).
	binaryBytes, binErr := os.ReadFile(resolvedExecPath)
	if binErr != nil {
		slog.Warn("DB (BBOLT): sidecar binary not found, skipping",
			"exec_path", resolvedExecPath, "err", binErr)
		return nil
	}

	// Default runtime to "binary" when absent.
	runtime := "binary"
	if sm.Runtime != "" {
		runtime = sm.Runtime
	}

	sourceHash := ComputeSidecarSourceHash(sm.Version, manifestBytes, binaryBytes)

	agentManifest := ManifestRecord{
		Version:          sm.Version,
		SupportedFormats: sm.SupportedFormats,
		Trait:            "tool",
	}
	manifestData, err := json.Marshal(agentManifest)
	if err != nil {
		return fmt.Errorf("marshal sidecar manifest for %s: %w", id, err)
	}
	if err := manifestsBucket.Put([]byte(id), manifestData); err != nil {
		return fmt.Errorf("put sidecar manifest for %s: %w", id, err)
	}

	existingData := agentsBucket.Get([]byte(id))
	if existingData == nil {
		slog.Info("DB (BBOLT): seeding sidecar tool-agent", "id", id)
		agentsDirSlash := strings.TrimRight(filepath.ToSlash(agentsDir), "/") + "/"
		sidecarExecPath := strings.TrimPrefix(resolvedExecPath, agentsDirSlash)
		agent := AgentRecord{
			ID:              id,
			Name:            id,
			Description:     sm.Description,
			Runtime:         runtime,
			ExecPath:        sidecarExecPath,
			Dir:             filepath.ToSlash(agentsDir),
			SourceHash:      sourceHash,
			ManifestVersion: sm.Version,
			Provisional:     true,
			Trait:           "tool",
			System:          isSystemAgent(id),
		}
		data, err := json.Marshal(agent)
		if err != nil {
			return fmt.Errorf("marshal new agent %s: %w", id, err)
		}
		if err := agentsBucket.Put([]byte(id), data); err != nil {
			return fmt.Errorf("put new agent %s: %w", id, err)
		}
	} else {
		var existing AgentRecord
		if jsonErr := json.Unmarshal(existingData, &existing); jsonErr == nil {
			if existing.SourceHash != sourceHash {
				slog.Info("DB (BBOLT): sidecar tool-agent changed, marking Provisional", "id", id)
				existing.SourceHash = sourceHash
				existing.ManifestVersion = sm.Version
				existing.Provisional = true
				existing.Description = sm.Description
				existing.ExecPath = resolvedExecPath
				existing.Runtime = runtime
				existing.System = isSystemAgent(id)
				data, err := json.Marshal(existing)
				if err != nil {
					return fmt.Errorf("marshal updated agent %s: %w", id, err)
				}
				if err := agentsBucket.Put([]byte(id), data); err != nil {
					return fmt.Errorf("put updated agent %s: %w", id, err)
				}
			}
		}
	}
	return nil
}

// GetAgentRecord returns the raw AgentRecord for the given name or ID.
func (b *BBoltAdapter) GetAgentRecord(name string) (*AgentRecord, error) {
	var rec AgentRecord
	found := false

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(agentBucket)
		if bucket == nil {
			return fmt.Errorf("agents bucket not found")
		}
		data := bucket.Get([]byte(name))
		if data != nil {
			if err := json.Unmarshal(data, &rec); err == nil {
				found = true
				return nil
			}
		}
		return bucket.ForEach(func(k, v []byte) error {
			var a AgentRecord
			if json.Unmarshal(v, &a) == nil {
				if a.Name == name {
					rec = a
					found = true
				}
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("agent not found in database: %s", name)
	}
	return &rec, nil
}

// GetAllAgentRecords returns raw AgentRecords for every agent.
func (b *BBoltAdapter) GetAllAgentRecords() ([]AgentRecord, error) {
	var recs []AgentRecord

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(agentBucket)
		if bucket == nil {
			return fmt.Errorf("agents bucket not found")
		}
		return bucket.ForEach(func(_, v []byte) error {
			var a AgentRecord
			if err := json.Unmarshal(v, &a); err == nil {
				recs = append(recs, a)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return recs, nil
}

// GetManifestRecord retrieves the persisted ManifestRecord for the given agent ID.
func (b *BBoltAdapter) GetManifestRecord(agentID string) (*ManifestRecord, error) {
	var rec ManifestRecord

	err := b.db.View(func(tx *bbolt.Tx) error {
		mb := tx.Bucket(manifestBucket)
		if mb == nil {
			return nil
		}
		data := mb.Get([]byte(agentID))
		if data == nil {
			return nil
		}
		return json.Unmarshal(data, &rec)
	})
	if err != nil {
		return nil, fmt.Errorf("manifest read failed: %w", err)
	}
	return &rec, nil
}

// GetManifestRecordBatch returns ManifestRecords for all given IDs in a single read Tx.
func (b *BBoltAdapter) GetManifestRecordBatch(ids []string) (map[string]ManifestRecord, error) {
	result := make(map[string]ManifestRecord, len(ids))
	err := b.db.View(func(tx *bbolt.Tx) error {
		mb := tx.Bucket(manifestBucket)
		if mb == nil {
			return nil
		}
		for _, id := range ids {
			data := mb.Get([]byte(id))
			if data == nil {
				continue
			}
			var rec ManifestRecord
			if json.Unmarshal(data, &rec) == nil {
				result[id] = rec
			}
		}
		return nil
	})
	return result, err
}

// WriteTaskEventRecord persists a TaskEventRecord to the task_events bucket.
func (b *BBoltAdapter) WriteTaskEventRecord(event TaskEventRecord) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("task_event marshal: %w", err)
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(taskEventBucket)
		if bucket == nil {
			return fmt.Errorf("task_events bucket not found")
		}
		return bucket.Put([]byte(event.TaskID), data)
	})
}

// ReadTaskEventRecord returns the TaskEventRecord stored under taskID, or nil.
func (b *BBoltAdapter) ReadTaskEventRecord(taskID string) (*TaskEventRecord, error) {
	var rec TaskEventRecord
	found := false

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(taskEventBucket)
		if bucket == nil {
			return nil
		}
		data := bucket.Get([]byte(taskID))
		if data == nil {
			return nil
		}
		found = true
		return json.Unmarshal(data, &rec)
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return &rec, nil
}

// ReadTaskEventRecords returns all TaskEventRecords for the given (agentID, sourceHash) pair.
func (b *BBoltAdapter) ReadTaskEventRecords(agentID, sourceHash string) ([]TaskEventRecord, error) {
	var recs []TaskEventRecord

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(taskEventBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, v []byte) error {
			var e TaskEventRecord
			if err := json.Unmarshal(v, &e); err == nil {
				if e.AgentID == agentID && e.SourceHash == sourceHash {
					recs = append(recs, e)
				}
			}
			return nil
		})
	})
	return recs, err
}

// ReadAllTaskEventRecords returns every TaskEventRecord in the task_events bucket.
// Backs the ROUTE-05 bid-calibration extraction (agent, bid_confidence, verifier_score).
func (b *BBoltAdapter) ReadAllTaskEventRecords() ([]TaskEventRecord, error) {
	var recs []TaskEventRecord
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(taskEventBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, v []byte) error {
			var e TaskEventRecord
			if json.Unmarshal(v, &e) == nil {
				recs = append(recs, e)
			}
			return nil
		})
	})
	return recs, err
}

// ReadAllAgentIDs returns distinct "agentID:sourceHash" strings from the
// task_events bucket.
func (b *BBoltAdapter) ReadAllAgentIDs() ([]string, error) {
	seen := make(map[string]bool)
	var keys []string

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(taskEventBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, v []byte) error {
			var e TaskEventRecord
			if err := json.Unmarshal(v, &e); err == nil && e.AgentID != "" {
				key := e.AgentID + ":" + e.SourceHash
				if !seen[key] {
					seen[key] = true
					keys = append(keys, key)
				}
			}
			return nil
		})
	})
	return keys, err
}

// SetProvisional updates the Provisional flag for the given agent.
func (b *BBoltAdapter) SetProvisional(agentID string, provisional bool) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(agentBucket)
		if bucket == nil {
			return fmt.Errorf("agents bucket not found")
		}
		data := bucket.Get([]byte(agentID))
		if data == nil {
			return fmt.Errorf("agent %s not found", agentID)
		}
		var rec AgentRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		rec.Provisional = provisional
		updated, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(agentID), updated)
	})
}

// SetCapabilities updates the Capabilities slice for the given agent in the agents bucket.
// Returns an error if the agent does not exist.
func (b *BBoltAdapter) SetCapabilities(agentID string, caps []string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(agentBucket)
		if bucket == nil {
			return fmt.Errorf("agents bucket not found")
		}
		data := bucket.Get([]byte(agentID))
		if data == nil {
			return fmt.Errorf("agent %s not found", agentID)
		}
		var rec AgentRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		rec.Capabilities = caps
		updated, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(agentID), updated)
	})
}

// SetClusterName writes a LLM-generated cluster name to the capability_clusters
// bucket keyed by representativeAgentID.
func (b *BBoltAdapter) SetClusterName(representativeAgentID string, clusterName string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(clusterBucket)
		if bucket == nil {
			return fmt.Errorf("capability_clusters bucket not found")
		}
		return bucket.Put([]byte(representativeAgentID), []byte(clusterName))
	})
}

// GetClusterName retrieves the cluster name for the given representativeAgentID.
// Returns ("", nil) when the key is absent — absence is not an error.
func (b *BBoltAdapter) GetClusterName(representativeAgentID string) (string, error) {
	var name string
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(clusterBucket)
		if bucket == nil {
			return nil
		}
		v := bucket.Get([]byte(representativeAgentID))
		if v != nil {
			name = string(v)
		}
		return nil
	})
	return name, err
}

// SaveCheckpoint persists a context checkpoint to the checkpoints bucket.
// Implements substrate.CheckpointStore.
func (b *BBoltAdapter) SaveCheckpoint(sessionID, planID string, stepIndex int, ctx map[string]string) error {
	rec := struct {
		SessionID string            `json:"session_id"`
		PlanID    string            `json:"plan_id"`
		StepIndex int               `json:"step_index"`
		Context   map[string]string `json:"context"`
		Timestamp time.Time         `json:"timestamp"`
	}{
		SessionID: sessionID,
		PlanID:    planID,
		StepIndex: stepIndex,
		Context:   ctx,
		Timestamp: time.Now(),
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(checkpointBucket)
		key := []byte(fmt.Sprintf("%s:%s:%d", sessionID, planID, stepIndex))
		data, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return bucket.Put(key, data)
	})
}

// LoadCheckpoint loads a specific checkpoint from BBolt.
func (b *BBoltAdapter) LoadCheckpoint(sessionID, planID string, stepIndex int) (map[string]string, error) {
	key := []byte(fmt.Sprintf("%s:%s:%d", sessionID, planID, stepIndex))
	var ctx map[string]string
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(checkpointBucket)
		if bucket == nil {
			return nil
		}
		data := bucket.Get(key)
		if data == nil {
			return nil
		}
		var rec struct {
			Context map[string]string `json:"context"`
		}
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		ctx = rec.Context
		return nil
	})
	return ctx, err
}

// ListCheckpoints returns all checkpoint metadata for a session.
func (b *BBoltAdapter) ListCheckpoints(sessionID string) ([]struct {
	SessionID string
	PlanID    string
	StepIndex int
	Timestamp time.Time
}, error) {
	prefix := []byte(sessionID + ":")
	type cpMeta struct {
		SessionID string    `json:"session_id"`
		PlanID    string    `json:"plan_id"`
		StepIndex int       `json:"step_index"`
		Timestamp time.Time `json:"timestamp"`
	}
	var out []cpMeta
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(checkpointBucket)
		if bucket == nil {
			return nil
		}
		c := bucket.Cursor()
		for k, v := c.Seek(prefix); k != nil && strings.HasPrefix(string(k), string(prefix)); k, v = c.Next() {
			var rec cpMeta
			if json.Unmarshal(v, &rec) != nil {
				continue
			}
			out = append(out, rec)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result := make([]struct {
		SessionID string
		PlanID    string
		StepIndex int
		Timestamp time.Time
	}, len(out))
	for i, m := range out {
		result[i] = struct {
			SessionID string
			PlanID    string
			StepIndex int
			Timestamp time.Time
		}{m.SessionID, m.PlanID, m.StepIndex, m.Timestamp}
	}
	return result, nil
}

// WriteAgentRecord persists an AgentRecord to the agents bucket.
func (b *BBoltAdapter) WriteAgentRecord(rec AgentRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("agent marshal: %w", err)
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(agentBucket)
		if bucket == nil {
			return fmt.Errorf("agents bucket not found")
		}
		return bucket.Put([]byte(rec.ID), data)
	})
}

// DeleteAgentRecord removes an agent from the agents bucket and its companion
// entry from the manifests bucket, in a single transaction. It is the storage
// primitive behind the startup registry reconcile: a model dropped from config
// or an agent whose source file was deleted must not linger as an auction
// candidate (GetAllAgents reads this bucket). Idempotent by design — a missing
// id is not an error, so reconcile can delete a computed orphan set without a
// prior existence check.
func (b *BBoltAdapter) DeleteAgentRecord(id string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(agentBucket)
		if bucket == nil {
			return fmt.Errorf("agents bucket not found")
		}
		if err := bucket.Delete([]byte(id)); err != nil {
			return err
		}
		// Best-effort manifest cleanup: the manifest bucket may be absent in
		// minimal stores (e.g. model-only registries), which is not an error.
		if mb := tx.Bucket(manifestBucket); mb != nil {
			return mb.Delete([]byte(id))
		}
		return nil
	})
}

// SaveArtifactRecord persists an ArtifactRecord (metadata + tags) to the
// artifacts bucket, keyed by content hash. ADR-0034 / REQ-SDK-007c.
func (b *BBoltAdapter) SaveArtifactRecord(rec ArtifactRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("artifact marshal: %w", err)
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(artifactBucket)
		if bucket == nil {
			return fmt.Errorf("artifacts bucket not found")
		}
		return bucket.Put([]byte(rec.Hash), data)
	})
}

// GetArtifactRecord reads an ArtifactRecord by content hash.
func (b *BBoltAdapter) GetArtifactRecord(hash string) (*ArtifactRecord, error) {
	var rec *ArtifactRecord
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(artifactBucket)
		if bucket == nil {
			return nil
		}
		data := bucket.Get([]byte(hash))
		if data == nil {
			return nil
		}
		rec = &ArtifactRecord{}
		return json.Unmarshal(data, rec)
	})
	return rec, err
}

// ListArtifactRecordsByStep returns all artifacts produced by a given session+step.
func (b *BBoltAdapter) ListArtifactRecordsByStep(sessionID string, stepIndex int) ([]ArtifactRecord, error) {
	var recs []ArtifactRecord
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(artifactBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, v []byte) error {
			var rec ArtifactRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return nil // skip malformed
			}
			if rec.SessionID == sessionID && rec.StepIndex == stepIndex {
				recs = append(recs, rec)
			}
			return nil
		})
	})
	return recs, err
}

// SaveSessionRecord persists a SessionRecord to the sessions bucket.
func (b *BBoltAdapter) SaveSessionRecord(rec SessionRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("session marshal: %w", err)
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(sessionBucket)
		if bucket == nil {
			return fmt.Errorf("sessions bucket not found")
		}
		return bucket.Put([]byte(rec.ID), data)
	})
}

// GetSessionRecord reads a SessionRecord by ID from the sessions bucket.
func (b *BBoltAdapter) GetSessionRecord(id string) (*SessionRecord, error) {
	var rec *SessionRecord
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(sessionBucket)
		if bucket == nil {
			return nil
		}
		data := bucket.Get([]byte(id))
		if data == nil {
			return nil
		}
		rec = &SessionRecord{}
		return json.Unmarshal(data, rec)
	})
	return rec, err
}

// ListSessionRecords returns all sessions, optionally filtered by status.
func (b *BBoltAdapter) ListSessionRecords(status string) ([]SessionRecord, error) {
	var recs []SessionRecord
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(sessionBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(k, v []byte) error {
			var rec SessionRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return nil // skip malformed records
			}
			if status == "" || rec.Status == status {
				recs = append(recs, rec)
			}
			return nil
		})
	})
	return recs, err
}

// WriteEventRecord persists an EventRecord to the events bucket.
func (b *BBoltAdapter) WriteEventRecord(rec EventRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("event marshal: %w", err)
	}
	key := []byte(fmt.Sprintf("%s:%s", rec.SessionID, rec.Timestamp))
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(eventBucket)
		if bucket == nil {
			return fmt.Errorf("events bucket not found")
		}
		return bucket.Put(key, data)
	})
}

// ListEventRecords returns up to limit events for a session (newest first).
func (b *BBoltAdapter) ListEventRecords(sessionID string, limit int) ([]EventRecord, error) {
	var recs []EventRecord
	prefix := []byte(sessionID + ":")
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(eventBucket)
		if bucket == nil {
			return nil
		}
		c := bucket.Cursor()
		for k, v := c.Seek(prefix); k != nil && strings.HasPrefix(string(k), string(prefix)); k, v = c.Next() {
			var rec EventRecord
			if json.Unmarshal(v, &rec) != nil {
				continue
			}
			recs = append(recs, rec)
		}
		return nil
	})
	if limit > 0 && len(recs) > limit {
		recs = recs[len(recs)-limit:]
	}
	return recs, err
}

// ListEventRecordsByType returns events of specified types for a session.
func (b *BBoltAdapter) ListEventRecordsByType(sessionID string, types []string) ([]EventRecord, error) {
	typeSet := make(map[string]bool, len(types))
	for _, t := range types {
		typeSet[t] = true
	}
	var recs []EventRecord
	prefix := []byte(sessionID + ":")
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(eventBucket)
		if bucket == nil {
			return nil
		}
		c := bucket.Cursor()
		for k, v := c.Seek(prefix); k != nil && strings.HasPrefix(string(k), string(prefix)); k, v = c.Next() {
			var rec EventRecord
			if json.Unmarshal(v, &rec) != nil {
				continue
			}
			if typeSet[rec.Type] {
				recs = append(recs, rec)
			}
		}
		return nil
	})
	return recs, err
}

// ListAllEventRecordsSince returns all events stored after the given time, up to limit.
// It does a full scan of the events bucket since keys are not globally time-ordered.
func (b *BBoltAdapter) ListAllEventRecordsSince(since time.Time, limit int) ([]EventRecord, error) {
	sinceStr := since.Format(time.RFC3339Nano)
	var recs []EventRecord
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(eventBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, v []byte) error {
			var rec EventRecord
			if json.Unmarshal(v, &rec) != nil {
				return nil
			}
			if rec.Timestamp >= sinceStr {
				recs = append(recs, rec)
			}
			return nil
		})
	})
	if limit > 0 && len(recs) > limit {
		recs = recs[len(recs)-limit:]
	}
	return recs, err
}

// ── PlanEvent ────────────────────────────────────────────────────────────────

func (b *BBoltAdapter) WritePlanEventRecord(event PlanEventRecord) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("plan_event marshal: %w", err)
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(planEventBucket)
		if bucket == nil {
			return fmt.Errorf("plan_events bucket not found")
		}
		return bucket.Put([]byte(event.PlanID), data)
	})
}

func (b *BBoltAdapter) ReadPlanEventRecord(planID string) (*PlanEventRecord, error) {
	var rec PlanEventRecord
	found := false
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(planEventBucket)
		if bucket == nil {
			return nil
		}
		data := bucket.Get([]byte(planID))
		if data == nil {
			return nil
		}
		found = true
		return json.Unmarshal(data, &rec)
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return &rec, nil
}

// ── RetrievalSession ─────────────────────────────────────────────────────────

func (b *BBoltAdapter) WriteRetrievalSessionRecord(session RetrievalSessionRecord) error {
	data, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("retrieval_session marshal: %w", err)
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(retrievalSessionBucket)
		if bucket == nil {
			return fmt.Errorf("retrieval_sessions bucket not found")
		}
		return bucket.Put([]byte(session.SessionID), data)
	})
}

func (b *BBoltAdapter) ReadRetrievalSessionRecord(sessionID string) (*RetrievalSessionRecord, error) {
	var rec RetrievalSessionRecord
	found := false
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(retrievalSessionBucket)
		if bucket == nil {
			return nil
		}
		data := bucket.Get([]byte(sessionID))
		if data == nil {
			return nil
		}
		found = true
		return json.Unmarshal(data, &rec)
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return &rec, nil
}

func (b *BBoltAdapter) UpdateRetrievalSessionPlanOutcome(sessionID, planID, outcome string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(retrievalSessionBucket)
		if bucket == nil {
			return fmt.Errorf("retrieval_sessions bucket not found")
		}
		data := bucket.Get([]byte(sessionID))
		if data == nil {
			return nil
		}
		var rec RetrievalSessionRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		rec.PlanID = planID
		rec.PlanOutcome = outcome
		updated, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(sessionID), updated)
	})
}

// ── TraversalLogEntry ────────────────────────────────────────────────────────

func (b *BBoltAdapter) WriteTraversalLogEntryRecord(entry TraversalLogEntryRecord) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("traversal_log_entry marshal: %w", err)
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(traversalLogBucket)
		if bucket == nil {
			return fmt.Errorf("traversal_log bucket not found")
		}
		return bucket.Put([]byte(entry.EntryID), data)
	})
}

func (b *BBoltAdapter) UpdateTraversalLogPlanOutcome(entryID, planID, outcome string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(traversalLogBucket)
		if bucket == nil {
			return fmt.Errorf("traversal_log bucket not found")
		}
		data := bucket.Get([]byte(entryID))
		if data == nil {
			return nil
		}
		var rec TraversalLogEntryRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		rec.PlanID = planID
		rec.PlanOutcome = outcome
		updated, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(entryID), updated)
	})
}

// ── ContradictionResolution ──────────────────────────────────────────────────

func (b *BBoltAdapter) WriteContradictionResolutionRecord(res ContradictionResolutionRecord) error {
	data, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("contradiction_resolution marshal: %w", err)
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(contradictionResolutionBucket)
		if bucket == nil {
			return fmt.Errorf("contradiction_resolutions bucket not found")
		}
		return bucket.Put([]byte(res.ResolutionID), data)
	})
}

// To clear the database lock on system shutdown
func (b *BBoltAdapter) Close() error {
	return b.db.Close()
}

// NewStepCache creates a BBoltStepCache backed by the same database file.
// The step_cache bucket is created if it does not already exist.
func (b *BBoltAdapter) NewStepCache() (*BBoltStepCache, error) {
	return NewBBoltStepCache(b.db)
}

// ── WatchConfig ───────────────────────────────────────────────────────────────

// WriteWatchConfig persists a WatchConfigRecord keyed by its ID. ADR-0032.
func (b *BBoltAdapter) WriteWatchConfig(rec WatchConfigRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("watch_config marshal: %w", err)
	}
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(watchConfigBucket)
		if bucket == nil {
			return fmt.Errorf("watch_configs bucket not found")
		}
		return bucket.Put([]byte(rec.ID), data)
	})
}

// ReadWatchConfig returns the WatchConfigRecord stored under id, or an error if absent.
func (b *BBoltAdapter) ReadWatchConfig(id string) (*WatchConfigRecord, error) {
	var rec WatchConfigRecord
	found := false

	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(watchConfigBucket)
		if bucket == nil {
			return nil
		}
		data := bucket.Get([]byte(id))
		if data == nil {
			return nil
		}
		found = true
		return json.Unmarshal(data, &rec)
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("watch_config %q not found", id)
	}
	return &rec, nil
}

// ReadAllWatchConfigs returns all WatchConfigRecords from the bucket.
func (b *BBoltAdapter) ReadAllWatchConfigs() ([]WatchConfigRecord, error) {
	var recs []WatchConfigRecord
	err := b.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(watchConfigBucket)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(k, v []byte) error {
			var rec WatchConfigRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			recs = append(recs, rec)
			return nil
		})
	})
	return recs, err
}

// DeleteWatchConfig removes the WatchConfigRecord with the given id.
// Returns an error if the id does not exist.
func (b *BBoltAdapter) DeleteWatchConfig(id string) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(watchConfigBucket)
		if bucket == nil {
			return fmt.Errorf("watch_configs bucket not found")
		}
		if bucket.Get([]byte(id)) == nil {
			return fmt.Errorf("watch_config %q not found", id)
		}
		return bucket.Delete([]byte(id))
	})
}

// SetWatchConfigActive updates only the Active field of an existing WatchConfigRecord.
func (b *BBoltAdapter) SetWatchConfigActive(id string, active bool) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(watchConfigBucket)
		if bucket == nil {
			return fmt.Errorf("watch_configs bucket not found")
		}
		data := bucket.Get([]byte(id))
		if data == nil {
			return fmt.Errorf("watch_config %q not found", id)
		}
		var rec WatchConfigRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		rec.Active = active
		updated, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(id), updated)
	})
}
