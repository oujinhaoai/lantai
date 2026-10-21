package client

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

const ImportReportSchema = "lantai.asset-import-report/v1"

const ImportHistoricalReceipts = "historical_receipts"

type ImportResult struct {
	Key         string `json:"key"`
	Status      string `json:"status"`
	AssetID     string `json:"asset_id,omitempty"`
	VersionID   string `json:"version_id,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	ErrorCode   string `json:"error_code,omitempty"`
}
type ImportReport struct {
	Schema                string         `json:"schema"`
	Origin                string         `json:"origin"`
	PlanDigest            string         `json:"plan_digest"`
	Complete              bool           `json:"complete"`
	CompletionBasis       string         `json:"completion_basis"`
	CurrentRemoteVerified bool           `json:"current_remote_verified"`
	Results               []ImportResult `json:"results"`
}

func (r *ImportReport) UnmarshalJSON(b []byte) error {
	type plain ImportReport
	var value plain
	if err := importStrictJSON(b, &value); err != nil {
		return err
	}
	if value.CurrentRemoteVerified || (value.CompletionBasis != "" && value.CompletionBasis != ImportHistoricalReceipts) {
		return errors.New("client: import receipt cannot claim current remote verification")
	}
	*r = ImportReport(value)
	// v1 reports predating this field also describe historical receipts. Never
	// infer current server availability from a saved local result.
	if r.CompletionBasis == "" {
		r.CompletionBasis = ImportHistoricalReceipts
	}
	return nil
}

// ApplyImport is the explicit mutation boundary. It never creates a replacement
// Push state on failure, grants permissions, publishes versions or deletes a
// source. A partial report is returned with any error and saved after each item.
func (c *Client) ApplyImport(ctx context.Context, plan ImportPlan, sourceRoot, stateDirectory string) (ImportReport, error) {
	report := ImportReport{Schema: ImportReportSchema, Origin: c.Origin(), PlanDigest: plan.Digest, CompletionBasis: ImportHistoricalReceipts, Results: []ImportResult{}}
	if err := ValidateImportPlan(ctx, plan, sourceRoot); err != nil {
		return report, err
	}
	stateDir, err := importAbsolute(stateDirectory)
	if err != nil {
		return report, err
	}
	if err = importDisjoint(plan.SourceRoot, stateDir); err != nil {
		return report, err
	}
	if err = importPrivateDirectory(stateDir); err != nil {
		return report, err
	}
	// Pin the batch directory before loading or writing any state. Every asset
	// state shares this root, so replacing its path during a request cannot
	// redirect recovery keys or the batch report into the source directory.
	stateFile, err := OpenPinnedPrivateOutput(filepath.Join(stateDir, "import.json"))
	if err != nil {
		return report, err
	}
	defer stateFile.Close()
	unlock, err := stateFile.lockState()
	if err != nil {
		return report, err
	}
	defer unlock()
	if err = stateFile.loadState(&report); errors.Is(err, os.ErrNotExist) {
		for _, a := range plan.Input.Assets {
			report.Results = append(report.Results, ImportResult{Key: a.Key, Status: "not_attempted"})
		}
		if err = stateFile.saveState(report); err != nil {
			return report, err
		}
	} else if err != nil {
		return report, err
	}
	if err = c.validateImportReport(plan, report); err != nil {
		return report, err
	}
	var failures []error
	for i, a := range plan.Input.Assets {
		err = nil
		if err = ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		// Rehash before every write/resume, including runs whose earlier item was
		// committed. Never silently send changed content under frozen keys.
		dir := filepath.Join(plan.SourceRoot, filepath.FromSlash(a.Directory))
		for _, f := range a.Push.Content.Files {
			if err = importNoLinks(filepath.Join(dir, filepath.FromSlash(f.Path)), false); err != nil {
				break
			}
		}
		if err == nil {
			_, err = PreparePush(ctx, a.Push, dir)
		}
		if err != nil {
			failures = append(failures, err)
			break
		}
		pushFile, stateErr := stateFile.sibling(a.Key + ".push.json")
		if stateErr != nil {
			return report, stateErr
		}
		defer pushFile.Close()
		// A receipt saved by Push records the historical acceptance even when the
		// process stopped before saving the batch row. It does not prove that a
		// restored server still contains this version. Reuse the original keys;
		// current remote presence requires VerifyImport with read permission.
		var saved PushState
		var response Response
		loadErr := pushFile.loadState(&saved)
		if loadErr == nil && len(saved.Result) > 0 {
			prepared, _ := json.Marshal(a.Push)
			hash := importSHA(prepared)
			if saved.Schema != "lantai.client-push/v1" || saved.Origin != c.Origin() || saved.RequestHash != hash || saved.CreateKey == "" || saved.CommitKey == "" {
				err = errors.New("client: import push state does not match the frozen request")
			} else {
				response = Response{Body: saved.Result}
				cached, receiptErr := importReceipt(a.Key, saved.Result)
				if receiptErr != nil || cached.OperationID != saved.OperationID || cached.Status == "description_pending" {
					response, err = c.pushWithStateFile(ctx, a.Push, dir, pushFile, false)
				}
			}
		} else if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
			err = loadErr
		} else if report.Results[i].Status != "not_attempted" && errors.Is(loadErr, os.ErrNotExist) {
			// Missing original keys can never be recovered by inventing a new push.
			err = errors.New("client: committed import item lost its original push state")
		} else {
			if report.Results[i].Status == "not_attempted" {
				report.Results[i].Status = "unknown"
				if err = stateFile.saveState(report); err != nil {
					return report, err
				}
			}
			response, err = c.pushWithStateFile(ctx, a.Push, dir, pushFile, false)
		}
		result := ImportResult{Key: a.Key}
		if err == nil {
			result, err = importReceipt(a.Key, response.Body)
			if err == nil {
				var current PushState
				if stateErr := pushFile.loadState(&current); stateErr != nil {
					err = stateErr
				} else if current.OperationID != result.OperationID {
					err = errors.New("client: import receipt operation differs from original push state")
				}
				previous := report.Results[i]
				if importCommitted(previous.Status) && (previous.AssetID != result.AssetID || previous.VersionID != result.VersionID || previous.OperationID != result.OperationID) {
					err = errors.New("client: replay changed a committed import receipt")
				}
				if a.Push.AssetID != "" && result.AssetID != a.Push.AssetID {
					err = errors.New("client: append receipt changed the chosen asset")
				}
			}
		}
		if err != nil {
			result = ImportResult{Key: a.Key, Status: "unknown", ErrorCode: importErrorCode(err)}
			var current PushState
			if pushFile.loadState(&current) == nil && current.Origin == c.Origin() && ids.ID(current.OperationID).Valid() {
				result.OperationID = current.OperationID
			}
			var apiErr *APIError
			if errors.As(err, &apiErr) && !apiErr.Protocol && apiErr.Status >= 400 && apiErr.Status < 500 {
				result.Status = "failed"
			}
			// Preserve any known committed identity instead of downgrading it.
			if importCommitted(report.Results[i].Status) {
				result = report.Results[i]
			}
			failures = append(failures, errors.New("client: import item "+a.Key+" did not complete; reuse the original state"))
		}
		report.Results[i] = result
		report.Complete = importAllDescribed(report.Results)
		if saveErr := stateFile.saveState(report); saveErr != nil {
			return report, saveErr
		}
	}
	report.Complete = importAllDescribed(report.Results)
	for _, r := range report.Results {
		if r.Status == "description_pending" {
			failures = append(failures, errors.New("client: files are committed but initial metadata is pending; resume original state without starting another upload"))
			break
		}
	}
	return report, errors.Join(failures...)
}

func (c *Client) validateImportReport(plan ImportPlan, report ImportReport) error {
	if report.CurrentRemoteVerified || (report.CompletionBasis != "" && report.CompletionBasis != ImportHistoricalReceipts) {
		return errors.New("client: import receipt cannot claim current remote verification")
	}
	if report.Schema != ImportReportSchema || report.Origin != c.Origin() || report.PlanDigest != plan.Digest || len(report.Results) != len(plan.Input.Assets) {
		return errors.New("client: import report/state belongs to a different plan or server")
	}
	for i, r := range report.Results {
		if r.Key != plan.Input.Assets[i].Key {
			return errors.New("client: import report asset order differs from plan")
		}
		switch r.Status {
		case "committed", "description_pending":
			if !ids.ID(r.AssetID).Valid() || !ids.ID(r.VersionID).Valid() || !ids.ID(r.OperationID).Valid() {
				return errors.New("client: import receipt omitted exact identities")
			}
		case "not_attempted", "failed", "unknown":
		default:
			return errors.New("client: invalid import item status")
		}
	}
	if report.Complete != importAllDescribed(report.Results) {
		return errors.New("client: inconsistent import completion flag")
	}
	return nil
}
func importReceipt(key string, raw json.RawMessage) (ImportResult, error) {
	var receipt struct {
		AssetID            string       `json:"asset_id"`
		VersionID          string       `json:"version_id"`
		OperationID        string       `json:"operation_id"`
		CommittedAt        string       `json:"committed_at"`
		DescriptionPending bool         `json:"description_pending"`
		DescriptionError   errcode.Code `json:"description_error"`
	}
	if json.Unmarshal(raw, &receipt) != nil || !ids.ID(receipt.AssetID).Valid() || !ids.ID(receipt.VersionID).Valid() || !ids.ID(receipt.OperationID).Valid() || receipt.CommittedAt == "" {
		return ImportResult{}, errors.New("client: import response omitted a committed exact-version receipt")
	}
	result := ImportResult{Key: key, Status: "committed", AssetID: receipt.AssetID, VersionID: receipt.VersionID, OperationID: receipt.OperationID}
	if receipt.DescriptionPending {
		result.Status = "description_pending"
		if _, known := errcode.Lookup(receipt.DescriptionError); known {
			result.ErrorCode = string(receipt.DescriptionError)
		}
	}
	return result, nil
}
func importErrorCode(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if _, known := errcode.Lookup(apiErr.Body.Code); known {
			return string(apiErr.Body.Code)
		}
	}
	return "IMPORT_INCOMPLETE"
}
func importCommitted(status string) bool {
	return status == "committed" || status == "description_pending"
}
func importAllDescribed(results []ImportResult) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		if r.Status != "committed" {
			return false
		}
	}
	return true
}
