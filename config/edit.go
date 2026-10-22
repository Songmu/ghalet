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
	Scope       string `yaml:"scope"`
	RunnerGroup string `yaml:"runnerGroup"`
	ScaleSet    string `yaml:"scaleSet"`
}

// NewEditor reads one configuration mapping. When schema is non-nil, it also
// decodes known fields into schema before allowing edits.
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
		if err := yaml.NodeToValue(root, schema); err != nil {
			return nil, errors.New("configuration contains invalid fields; validate it before registering a credential")
		}
	}
	editor := &Editor{comments: yaml.CommentMap{}}
	if err := yaml.NodeToValue(root, &editor.identities); err != nil {
		return nil, errors.New("cannot decode target registrations for configuration update")
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

// AppendTarget adds a target that inherits the configured credential.
func (editor *Editor) AppendTarget(scope, group, scaleSet string) error {
	targets, err := editor.targets()
	if err != nil {
		return err
	}
	mapping := yaml.MapSlice{
		{Key: "scope", Value: scope}, {Key: "scaleSet", Value: scaleSet},
	}
	if group != "" {
		mapping = append(mapping, yaml.MapItem{Key: "runnerGroup", Value: group})
	}
	setMapping(&editor.document, "targets", append(targets, mapping))
	editor.identities.Targets = append(editor.identities.Targets, targetIdentity{Scope: scope, RunnerGroup: group, ScaleSet: scaleSet})
	return nil
}

// SetTargetApp replaces a target's credentials and installation.
func (editor *Editor) SetTargetApp(id, clientID, keyPath string, installationID int64) error {
	targets, err := editor.targets()
	if err != nil {
		return err
	}
	if len(targets) != len(editor.identities.Targets) {
		return errors.New("configuration target mappings do not match decoded registrations")
	}
	found := false
	for i, value := range targets {
		mapping, ok := value.(yaml.MapSlice)
		if !ok {
			return errors.New("configuration target must be a mapping")
		}
		target := editor.identities.Targets[i]
		key, err := TargetKey(target.Scope, target.RunnerGroup, target.ScaleSet)
		if err != nil {
			return err
		}
		if key != id {
			continue
		}
		credential := appCredential(clientID, keyPath)
		setMapping(&credential, "installationId", installationID)
		setMapping(&mapping, "credential", credential)
		targets[i] = mapping
		found = true
	}
	if !found {
		return fmt.Errorf("target %q disappeared from the configuration", id)
	}
	return nil
}

// SetDefaultCredential replaces defaults while leaving explicit targets intact.
func (editor *Editor) SetDefaultCredential(credentialType, clientID, path string, installationID int64) {
	credential := yaml.MapSlice{{Key: "type", Value: credentialType}, {Key: "tokenFile", Value: path}}
	if credentialType == "github-app" {
		credential = appCredential(clientID, path)
		setMapping(&credential, "installationId", installationID)
	}
	setMapping(&editor.document, "defaults", yaml.MapSlice{{Key: "credential", Value: credential}})
}

// RemoveDefaultCredential allows an explicitly selected managed default to apply.
func (editor *Editor) RemoveDefaultCredential() {
	for i := range editor.document {
		if editor.document[i].Key != "defaults" {
			continue
		}
		mapping, ok := editor.document[i].Value.(yaml.MapSlice)
		if !ok {
			continue
		}
		for j := range mapping {
			if mapping[j].Key == "credential" {
				editor.document[i].Value = append(mapping[:j], mapping[j+1:]...)
				return
			}
		}
	}
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
