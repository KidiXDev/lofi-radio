package config

import "gopkg.in/yaml.v3"

type Serializer interface {
	Marshal(value any) ([]byte, error)
	Unmarshal(data []byte, target any) error
}

type YAMLSerializer struct{}

func (YAMLSerializer) Marshal(value any) ([]byte, error) {
	return yaml.Marshal(value)
}

func (YAMLSerializer) Unmarshal(data []byte, target any) error {
	return yaml.Unmarshal(data, target)
}
