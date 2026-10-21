package config

import (
	"errors"
	"fmt"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// Editor updates ghalet configuration mappings without discarding their order
// or comments. Aliases are expanded so replacing defaults cannot change explicit
// target credentials that referred to the old defaults.
type Editor struct {
	document   yaml.MapSlice
	comments   yaml.CommentMap
	identities struct {
		Targets []targetIdentity `yaml:"targets"`
	}
}

type targetIdentity struct {
	ID string `yaml:"id"`
}

// NewEditor reads one configuration mapping. When schema is non-nil, it also
// strictly decodes the configuration into schema before allowing edits.
func NewEditor(data []byte, schema any) (*Editor, error) {
	file, err := parser.ParseBytes(data, parser.ParseComments)
	if err != nil {
		return nil, errors.New("cannot parse the selected configuration")
	}
	if len(file.Docs) != 1 {
		return nil, errors.New("configuration must contain exactly one YAML document")
	}
	root, ok := file.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		return nil, errors.New("configuration must be a YAML mapping")
	}
	if schema != nil {
		if err := yaml.NodeToValue(root, schema, yaml.Strict()); err != nil {
			return nil, errors.New("configuration contains invalid or unknown fields; validate it before registering a credential")
		}
	}
	editor := &Editor{comments: yaml.CommentMap{}}
	// Use the same string coercion as configuration loading, including unquoted
	// numeric IDs, instead of comparing the untyped ordered-map values.
	if err := yaml.NodeToValue(root, &editor.identities); err != nil {
		return nil, errors.New("cannot decode target IDs for configuration update")
	}
	if err := yaml.UnmarshalWithOptions(data, &editor.document, yaml.UseOrderedMap(), yaml.CommentToMap(editor.comments)); err != nil {
		return nil, errors.New("cannot read configuration mappings")
	}
	return editor, nil
}

func (editor *Editor) targets() ([]any, error) {
	for _, item := range editor.document {
		if item.Key == "targets" {
			targets, ok := item.Value.([]any)
			if !ok {
				return nil, errors.New("configuration targets must be a sequence")
			}
			return targets, nil
		}
	}
	return nil, errors.New("configuration must contain targets")
}

// AppendTarget adds a target with an explicit empty credential, avoiding
// inheritance of an unrelated default GitHub App installation.
func (editor *Editor) AppendTarget(id, scope string) error {
	targets, err := editor.targets()
	if err != nil {
		return err
	}
	setMapping(&editor.document, "targets", append(targets, yaml.MapSlice{
		{Key: "id", Value: id}, {Key: "scope", Value: scope},
		{Key: "credential", Value: yaml.MapSlice{}},
	}))
	editor.identities.Targets = append(editor.identities.Targets, targetIdentity{ID: id})
	return nil
}

// SetTargetApp replaces a target's credentials and installation.
func (editor *Editor) SetTargetApp(id, clientID, keyPath string, installationID int64) error {
	targets, err := editor.targets()
	if err != nil {
		return err
	}
	if len(targets) != len(editor.identities.Targets) {
		return errors.New("configuration target mappings do not match decoded target IDs")
	}
	found := false
	for i, value := range targets {
		mapping, ok := value.(yaml.MapSlice)
		if !ok {
			return errors.New("configuration target must be a mapping")
		}
		if editor.identities.Targets[i].ID != id {
			continue
		}
		setMapping(&mapping, "credential", appCredential(clientID, keyPath))
		setMapping(&mapping, "installationId", installationID)
		targets[i] = mapping
		found = true
	}
	if !found {
		return fmt.Errorf("target %q disappeared from the configuration", id)
	}
	return nil
}

// SetDefaultCredential replaces defaults while leaving explicit targets intact.
func (editor *Editor) SetDefaultCredential(credentialType, clientID, path string) {
	credential := yaml.MapSlice{{Key: "type", Value: credentialType}, {Key: "tokenFile", Value: path}}
	if credentialType == "github-app" {
		credential = appCredential(clientID, path)
	}
	setMapping(&editor.document, "defaults", yaml.MapSlice{{Key: "credential", Value: credential}})
}

func appCredential(clientID, keyPath string) yaml.MapSlice {
	return yaml.MapSlice{
		{Key: "type", Value: "github-app"},
		{Key: "clientId", Value: clientID},
		{Key: "privateKeyFile", Value: keyPath},
	}
}

// Marshal returns the edited configuration with its comments and mapping order.
func (editor *Editor) Marshal() ([]byte, error) {
	return yaml.MarshalWithOptions(editor.document, yaml.WithComment(editor.comments))
}

func setMapping(mapping *yaml.MapSlice, key string, value any) {
	for i := range *mapping {
		if (*mapping)[i].Key == key {
			(*mapping)[i].Value = value
			return
		}
	}
	*mapping = append(*mapping, yaml.MapItem{Key: key, Value: value})
}
