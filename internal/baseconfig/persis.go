// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package baseconfig

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"slices"

	"github.com/dagucloud/dagu/v2/internal/cmn/fileutil"
	"github.com/dagucloud/dagu/v2/internal/ir"
	"github.com/dagucloud/dagu/v2/internal/pagination"
	"github.com/dagucloud/dagu/v2/internal/persis"
	"github.com/dagucloud/dagu/v2/internal/spec"
	"github.com/dagucloud/dagu/v2/internal/workspace"
)

// PersisEnumerator lists affected workflows from a DAGRepository. The set is
// the same catalog the scheduler registry is built from.
type PersisEnumerator struct {
	repo *persis.DAGRepository
}

// NewPersisEnumerator creates an enumerator backed by a DAGRepository.
func NewPersisEnumerator(repo *persis.DAGRepository) *PersisEnumerator {
	return &PersisEnumerator{repo: repo}
}

// ListAffected implements Enumerator. Global scope lists every workflow; a
// workspace scope lists every file containing a document with that workspace
// label, including multi-document files whose root document is unlabelled.
func (e *PersisEnumerator) ListAffected(ctx context.Context, scope Scope) ([]WorkflowSource, error) {
	paginator := pagination.NewPaginator(1, math.MaxInt)
	result, _, err := e.repo.List(ctx, persis.DAGListOptions{Paginator: &paginator})
	if err != nil {
		return nil, err
	}

	workflows := make([]WorkflowSource, 0, len(result.Items))
	for _, item := range result.Items {
		source, err := e.repo.GetSpec(ctx, item.ID)
		if err != nil {
			return nil, fmt.Errorf("read workflow %q: %w", item.ID, err)
		}
		rawSource := []byte(source)

		rootWorkspace := ""
		location := ""
		name := item.ID
		if item.DAG != nil {
			if ws, ok := workspace.WorkspaceNameFromLabels(item.DAG.Labels); ok {
				rootWorkspace = ws
			}
			location = item.DAG.Location
			if item.DAG.Name != "" {
				name = item.DAG.Name
			}
		}

		// A file belongs to every workspace named in any of its documents.
		allWorkspaces := spec.WorkspaceLabelsFromDocuments(rawSource)
		if rootWorkspace != "" && !slices.Contains(allWorkspaces, rootWorkspace) {
			allWorkspaces = append([]string{rootWorkspace}, allWorkspaces...)
		}
		if !scope.IsGlobal() && !slices.Contains(allWorkspaces, scope.Workspace) {
			continue
		}

		workflows = append(workflows, WorkflowSource{
			WorkflowRef: WorkflowRef{
				Name:      name,
				Location:  location,
				Workspace: rootWorkspace,
			},
			Source:     rawSource,
			SourcePath: location,
			Workspaces: allWorkspaces,
		})
	}
	return workflows, nil
}

// SpecBuilder rebuilds workflows with the production spec pipeline. The load
// options mirror DAGRepository.GetDetails (WithoutEval, schema validation
// enabled); only the base config provenance is replaced with in-memory
// candidate content.
type SpecBuilder struct{}

// NewSpecBuilder returns the production Builder.
func NewSpecBuilder() SpecBuilder {
	return SpecBuilder{}
}

// Build implements Builder.
func (SpecBuilder) Build(
	ctx context.Context,
	workflow WorkflowSource,
	globalContent []byte,
	workspaceOverride func(workspaceName string) (content []byte, ok bool),
) error {
	options := []spec.LoadOption{
		spec.WithoutEval(),
		spec.WithBaseConfigContentSources(spec.BaseConfigContentSources{
			GlobalContent:     globalContent,
			WorkspaceOverride: workspaceOverride,
		}),
	}

	var dag *ir.DAG
	var err error
	if workflow.SourcePath != "" {
		options = append(options, spec.WithDefaultName(stemName(workflow.SourcePath, workflow.Name)))
		dag, err = spec.LoadYAMLAt(ctx, workflow.Source, workflow.SourcePath, options...)
	} else {
		options = append(options, spec.WithName(workflow.Name))
		dag, err = spec.LoadYAML(ctx, workflow.Source, options...)
	}
	if err != nil {
		return err
	}
	return dag.Validate()
}

func stemName(sourcePath, fallback string) string {
	stem := fileutil.TrimYAMLFileExtension(filepath.Base(sourcePath))
	if stem == "" || stem == "." || stem == string(filepath.Separator) {
		return fallback
	}
	return stem
}
