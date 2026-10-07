package modeldir

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// encodeModel renders a file. Key order is the Model field order; nothing time-dependent is written, so equal models encode to equal bytes.
func encodeModel(m Model) ([]byte, error) {
	var doc yaml.Node
	if err := doc.Encode(m); err != nil {
		return nil, fmt.Errorf("modeldir: encode %s: %w", m.Name, err)
	}
	compactValues(&doc)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("modeldir: encode %s: %w", m.Name, err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("modeldir: encode %s: %w", m.Name, err)
	}
	return buf.Bytes(), nil
}

// compactValues puts each nested value on one line, and each pricing rate on its own line, so a file reads as a short spec sheet.
func compactValues(doc *yaml.Node) {
	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		value := root.Content[i+1]
		if root.Content[i].Value == "pricing" {
			for _, rate := range value.Content {
				rate.Style = yaml.FlowStyle
			}
			continue
		}
		if value.Kind == yaml.MappingNode || value.Kind == yaml.SequenceNode {
			value.Style = yaml.FlowStyle
		}
	}
}

// decodeModel parses a file, rejecting unknown keys so a typo in a hand-written file fails loudly instead of dropping the field.
func decodeModel(data []byte) (Model, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var m Model
	if err := dec.Decode(&m); err != nil {
		if errors.Is(err, io.EOF) {
			return Model{}, errors.New("empty file")
		}
		return Model{}, err
	}
	return m, nil
}
