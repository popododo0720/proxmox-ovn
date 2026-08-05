package pve

import (
	"fmt"
	"strings"
)

// PropertyField is one comma-delimited field in a Proxmox QEMU property
// string. HasValue distinguishes "flag" from "flag=".
type PropertyField struct {
	Key      string
	Value    string
	HasValue bool
}

// NetProperty is an ordered, lossless representation of a QEMU netN property.
// Unknown fields are retained so PVN can change link_down without discarding
// options introduced by a newer Proxmox release.
type NetProperty struct {
	fields []PropertyField
}

// ParseNetProperty parses a Proxmox QEMU network property string.
func ParseNetProperty(value string) (NetProperty, error) {
	var property NetProperty
	if value == "" {
		return property, nil
	}

	for _, rawField := range strings.Split(value, ",") {
		if rawField == "" {
			return NetProperty{}, fmt.Errorf("empty network property field in %q", value)
		}
		key, fieldValue, found := strings.Cut(rawField, "=")
		if err := validatePropertyKey(key); err != nil {
			return NetProperty{}, err
		}
		property.fields = append(property.fields, PropertyField{Key: key, Value: fieldValue, HasValue: found})
	}
	return property, nil
}

// ParseQEMUNet is an explicit alias for callers that work with QEMU configs.
func ParseQEMUNet(value string) (NetProperty, error) { return ParseNetProperty(value) }

func (p NetProperty) String() string {
	fields := make([]string, 0, len(p.fields))
	for _, field := range p.fields {
		if field.HasValue {
			fields = append(fields, field.Key+"="+field.Value)
		} else {
			fields = append(fields, field.Key)
		}
	}
	return strings.Join(fields, ",")
}

// Fields returns a defensive copy of all fields in their original order.
func (p NetProperty) Fields() []PropertyField {
	return append([]PropertyField(nil), p.fields...)
}

func (p NetProperty) Clone() NetProperty {
	return NetProperty{fields: p.Fields()}
}

func (p NetProperty) Get(key string) (string, bool) {
	for _, field := range p.fields {
		if field.Key == key {
			return field.Value, true
		}
	}
	return "", false
}

// Set changes the first matching field in place and removes duplicate fields.
// A new field is appended, preserving all unrelated field order.
func (p *NetProperty) Set(key, value string) error {
	if err := validatePropertyKey(key); err != nil {
		return err
	}
	if strings.ContainsAny(value, ",\r\n") {
		return fmt.Errorf("invalid value for network property %q", key)
	}

	updated := make([]PropertyField, 0, len(p.fields)+1)
	found := false
	for _, field := range p.fields {
		if field.Key != key {
			updated = append(updated, field)
			continue
		}
		if !found {
			updated = append(updated, PropertyField{Key: key, Value: value, HasValue: true})
			found = true
		}
	}
	if !found {
		updated = append(updated, PropertyField{Key: key, Value: value, HasValue: true})
	}
	p.fields = updated
	return nil
}

func (p *NetProperty) Delete(key string) {
	updated := p.fields[:0]
	for _, field := range p.fields {
		if field.Key != key {
			updated = append(updated, field)
		}
	}
	p.fields = updated
}

func (p *NetProperty) SetLinkDown(down bool) error {
	value := "0"
	if down {
		value = "1"
	}
	return p.Set("link_down", value)
}

func validatePropertyKey(key string) error {
	if strings.TrimSpace(key) != key || key == "" || strings.ContainsAny(key, "=,\r\n") {
		return fmt.Errorf("invalid network property key %q", key)
	}
	return nil
}
