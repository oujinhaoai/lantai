package localassistant

import (
	"encoding/json"
	"path/filepath"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

const LocalResultRefSchema = "lantai.local-assistant-result-ref/v1"

type LocalResultRef struct {
	Schema       string `json:"schema"`
	JobID        string `json:"job_id"`
	ResultSHA256 string `json:"result_sha256"`
}
type workerArtifact struct {
	Schema         string `json:"schema"`
	JobID          string `json:"job_id"`
	ConfigDigest   string `json:"config_digest"`
	ProjectID      string `json:"project_id"`
	WorkspaceAlias string `json:"workspace_alias"`
	Result         Result `json:"result"`
}

func artifactName(jobID string) string { return "artifact-" + jobID + ".json" }
func decodeLocalRef(raw []byte) (LocalResultRef, bool, error) {
	var head struct {
		Schema string `json:"schema"`
	}
	if json.Unmarshal(raw, &head) != nil || head.Schema != LocalResultRefSchema {
		return LocalResultRef{}, false, nil
	}
	var ref LocalResultRef
	if strict(raw, &ref) != nil || !ids.ID(ref.JobID).Valid() || !digestPattern.MatchString(ref.ResultSHA256) {
		return ref, true, invalid("local result reference is invalid")
	}
	return ref, true, nil
}
func readArtifact(workspace string, ref LocalResultRef) (workerArtifact, error) {
	var a workerArtifact
	path := filepath.Join(workspace, workerPrivateDirectory, artifactName(ref.JobID))
	if err := noLinks(path, false); err != nil {
		return a, err
	}
	b, err := client.ReadPrivate(path)
	if err != nil || strict(b, &a) != nil || a.Schema != "lantai.assistant-worker-artifact/v1" || a.JobID != ref.JobID || a.Result.Schema != ResultSchema || hash(a.Result.Result) != ref.ResultSHA256 {
		return a, invalid("original private result artifact missing or changed; preserve original state")
	}
	return a, nil
}
func resolveLocalResult(workspace string, raw []byte, action string, report bool) (json.RawMessage, error) {
	ref, isRef, err := decodeLocalRef(raw)
	if err != nil || !isRef {
		return raw, err
	}
	artifact, err := readArtifact(workspace, ref)
	if err != nil {
		return nil, err
	}
	want := ""
	if report && action == "import.verify" {
		want = "import.apply"
	} else {
		switch action {
		case "import.apply", "import.verify":
			want = "import.plan"
		case "references.copy", "references.rollback":
			want = "references.plan"
		}
	}
	if want == "" || artifact.Result.Action != want || len(artifact.Result.Result) == 0 {
		return nil, invalid("local result reference does not match the selected action")
	}
	if !report && (artifact.Result.Status != "completed" || artifact.Result.ExitCode != 0) {
		return nil, invalid("input plan reference is not completed")
	}
	return artifact.Result.Result, nil
}

// InspectLocalResult is a read-only local review channel. The caller must carry
// the exact SHA from the observed network reference; no private path is accepted.
func InspectLocalResult(workspace, jobID, resultSHA256 string) (LocalResultInspection, error) {
	var out LocalResultInspection
	if workspace == "" {
		return out, invalid("explicit workspace required")
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return out, err
	}
	ref := LocalResultRef{Schema: LocalResultRefSchema, JobID: jobID, ResultSHA256: resultSHA256}
	if !ids.ID(jobID).Valid() || !digestPattern.MatchString(resultSHA256) {
		return out, invalid("exact remote job ID and original result SHA256 required")
	}
	artifact, err := readArtifact(abs, ref)
	if err != nil {
		return out, err
	}
	return LocalResultInspection{Schema: "lantai.local-assistant-result-inspection/v1", Reference: ref, Result: artifact.Result}, nil
}

type LocalResultInspection struct {
	Schema    string         `json:"schema"`
	Reference LocalResultRef `json:"local_result_ref"`
	Result    Result         `json:"result"`
}
