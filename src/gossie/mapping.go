package gossie

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

/*
	ideas:
	mapping for Go maps
	mapping for Go slices (N slices?)
*/

// Mapping maps the type of a Go object to/from a Cassandra row.
type Mapping interface {

	// Cf returns the column family name
	Cf() string

	// MarshalKey marshals the passed key value into a []byte
	MarshalKey(key interface{}) ([]byte, error)

	// MarshalComponent marshals the passed component value at the position into a []byte
	MarshalComponent(component interface{}, position int) ([]byte, error)

	// Map converts a Go object compatible with this Mapping into a Row
	Map(source interface{}) (*Row, error)

	// Ummap fills the passed Go object with data from a row
	Unmap(destination interface{}, provider RowProvider) error
}

var (
	EndBeforeLimit = errors.New("No more results found before reaching the limit")
	EndAtLimit     = errors.New("No more results found but reached the limit")
)

// RowProvider abstracts the details of reading a series of columns from a Cassandra row
type RowProvider interface {

	// Key returns the row key
	Key() ([]byte, error)

	// NextColumn returns the next column in the row, and advances the column pointer
	NextColumn() (*Column, error)

	// Rewind moves back the column pointer one position
	Rewind()
}

var (
	noMoreComponents = errors.New("No more components allowed")
)

// Converts a map to a cassandra row.
// TODO: consider implementing the Mapping interface.
func MapToRow(key string, m map[string]interface{}) (*Row, error) {
	timeStamp := now()
	serializedKey, err := Marshal(key, UTF8Type)
	if err != nil {
		return nil, errors.New(fmt.Sprint("Error marshaling key with value ", key, ":", err))
	}
	cols := make([]*Column, len(m))
	c := 0
	for k, v := range m {
		ctype := defaultType(reflect.TypeOf(v))
		serializedV, err := Marshal(v, ctype)
		if err != nil {
			return nil, errors.New(fmt.Sprint("Error marshaling field value for field ", k, ":", err))
		}
		serializedK, err := Marshal(k, UTF8Type)
		if err != nil {
			return nil, errors.New(fmt.Sprint("Error marshaling field name for field ", k, ":", err))
		}
		cols[c] = &Column{
			Name:      serializedK,
			Value:     serializedV,
			Ttl:       0,
			Timestamp: timeStamp,
		}
		c++
	}
	return &Row{
		Key:     serializedKey,
		Columns: cols,
	}, nil
}

// Converts a cassandra row to a map.
// Since the data comes out of cassandra as []byte, without any kind of type, we
// have to use a scheme map to obtain the type information from.
// What's the point of not using a struct when you still have to define a scheme as a map?
// The answer is maps can still be created at runtime, while structs must be known at compile time.
// This way, we can still handle data which structure is not known ahead of time (eg. or comes from a config).
func RowToMap(scheme map[string]interface{}, r *Row) (map[string]interface{}, error) {
	if r == nil {
		return nil, errors.New("RowToMap: row is nil")
	}
	ret := map[string]interface{}{}
	for _, col := range r.Columns {
		colName := string(col.Name)
		schVal, has := scheme[colName]
		if !has {
			// Ignore keys not present in scheme map
			continue
		}
		schValType := reflect.TypeOf(schVal)
		ctype := defaultType(schValType)
		mapVal := reflect.New(schValType).Interface()
		err := Unmarshal(col.Value, ctype, mapVal)
		if err != nil {
			return nil, errors.New(fmt.Sprint("Error marshaling field value for field ", colName, ":", err))
		}
		mapKey := ""
		err = Unmarshal(col.Name, UTF8Type, &mapKey)
		if err != nil {
			return nil, errors.New(fmt.Sprint("Error marshaling field name for field ", colName, ":", err))
		}
		ret[mapKey] = reflect.Indirect(reflect.ValueOf(mapVal)).Interface()
	}
	return ret, nil
}

// NewMapping looks up the field tag 'mapping' in the passed struct type
// to decide which mapping it is using, then builds a mapping using the 'cf',
// 'key', 'cols' and 'value' field tags.
func NewMapping(source interface{}) (Mapping, error) {
	_, si, err := validateAndInspectStruct(source)
	if err != nil {
		return nil, err
	}

	cf, found := si.globalTags["cf"]
	if !found {
		return nil, errors.New(fmt.Sprint("Mandatory struct tag 'cf' not found in passed struct of type ", si.rtype.Name()))
	}

	key, found := si.globalTags["key"]
	if !found {
		return nil, errors.New(fmt.Sprint("Mandatory struct tag 'key' not found in passed struct of type ", si.rtype.Name()))
	}
	_, found = si.goFields[key]
	if !found {
		return nil, errors.New(fmt.Sprint("Key field ", key, " not found in passed struct of type ", si.rtype.Name()))
	}

	colsS := []string{}
	cols, found := si.globalTags["cols"]
	if found {
		colsS = strings.Split(cols, ",")
	}
	for _, c := range colsS {
		_, found := si.goFields[c]
		if !found {
			return nil, errors.New(fmt.Sprint("Composite field ", c, " not found in passed struct of type ", si.rtype.Name()))
		}
	}

	value, found := si.globalTags["value"]
	if found {
		_, found := si.goFields[value]
		if !found {
			return nil, errors.New(fmt.Sprint("Value field ", value, " not found in passed struct of type ", si.rtype.Name()))
		}
	}

	mapping, found := si.globalTags["mapping"]
	if !found {
		mapping = "sparse"
	}

	switch mapping {
	case "sparse":
		return newSparseMapping(si, cf, key, colsS...), nil
	case "compact":
		return newCompactMapping(si, cf, key, value, colsS...), nil
	}

	return nil, errors.New(fmt.Sprint("Unrecognized mapping type ", mapping, " in passed struct of type ", si.rtype.Name()))
}

func newSparseMapping(si *structInspection, cf string, keyField string, componentFields ...string) Mapping {
	cm := make(map[string]bool, 0)
	for _, f := range componentFields {
		cm[f] = true
	}
	return &sparseMapping{
		si:            si,
		cf:            cf,
		key:           keyField,
		components:    componentFields,
		componentsMap: cm,
	}
}

type sparseMapping struct {
	si            *structInspection
	cf            string
	key           string
	components    []string
	componentsMap map[string]bool
}

func (m *sparseMapping) Cf() string {
	return m.cf
}

func (m *sparseMapping) MarshalKey(key interface{}) ([]byte, error) {
	f := m.si.goFields[m.key]
	b, err := Marshal(key, f.cassandraType)
	if err != nil {
		return nil, errors.New(fmt.Sprint("Error marshaling passed value for the key in field ", f.name, ":", err))
	}
	return b, nil
}

func (m *sparseMapping) MarshalComponent(component interface{}, position int) ([]byte, error) {
	if position >= len(m.components) {
		return nil, errors.New(fmt.Sprint("The mapping has a component length of ", len(m.components), " and the passed position is ", position))
	}
	f := m.si.goFields[m.components[position]]
	b, err := Marshal(component, f.cassandraType)
	if err != nil {
		return nil, errors.New(fmt.Sprint("Error marshaling passed value for a composite component in field ", f.name, ":", err))
	}
	return b, nil
}

func (m *sparseMapping) startMap(source interface{}) (*Row, *reflect.Value, *structInspection, []byte, error) {
	v, si, err := validateAndInspectStruct(source)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	row := &Row{}

	// marshal the key field
	if f, found := si.goFields[m.key]; found {
		b, err := f.marshalValue(v)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		row.Key = b
	} else {
		return nil, nil, nil, nil, errors.New(fmt.Sprint("Mapping key field ", m.key, " not found in passed struct of type ", v.Type().Name()))
	}

	// prepare composite, if needed
	composite := make([]byte, 0)
	for _, c := range m.components {
		if f, found := si.goFields[c]; found {
			b, err := f.marshalValue(v)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			composite = append(composite, packComposite(b, eocEquals)...)
		} else {
			return nil, nil, nil, nil, errors.New(fmt.Sprint("Mapping component field ", c, " not found in passed struct of type ", v.Type().Name()))
		}
	}

	return row, v, si, composite, nil
}

func (m *sparseMapping) Map(source interface{}) (*Row, error) {
	row, v, si, composite, err := m.startMap(source)
	if err != nil {
		return nil, err
	}

	// add columns
	for _, f := range si.orderedFields {
		if f.name == m.key {
			continue
		}
		if _, found := m.componentsMap[f.name]; found {
			continue
		}
		columnName, err := f.marshalName()
		if err != nil {
			return nil, err
		}
		totalLen := len(columnName)
		if len(composite) > 0 {
			// composite columns uses one byte for the separator
			// and two for storing the size of the data
			totalLen += len(composite) + 3
		}
		cp := make([]byte, 0, totalLen)
		if len(composite) > 0 {
			cp = append(cp, composite...)
			cp = append(cp, packComposite(columnName, eocEquals)...)
		} else {
			cp = append(cp, columnName...)
		}
		columnValue, err := f.marshalValue(v)
		if err != nil {
			return nil, err
		}
		row.Columns = append(row.Columns, &Column{Name: cp, Value: columnValue})
	}

	return row, nil
}

func (m *sparseMapping) startUnmap(destination interface{}, provider RowProvider) (*reflect.Value, *structInspection, error) {
	v, si, err := validateAndInspectStruct(destination)
	if err != nil {
		return nil, nil, err
	}

	// unmarshal key field
	if f, found := si.goFields[m.key]; found {
		key, err := provider.Key()
		if err != nil {
			return nil, nil, err
		}
		err = f.unmarshalValue(key, v)
		if err != nil {
			return nil, nil, err
		}
	} else {
		return nil, nil, errors.New(fmt.Sprint("Mapping key field ", m.key, " not found in passed struct of type ", v.Type().Name()))
	}

	return v, si, nil
}

func (m *sparseMapping) unmapComponents(v *reflect.Value, si *structInspection, components [][]byte) error {
	for i, c := range m.components {
		if f, found := si.goFields[c]; found {
			b := components[i]
			err := f.unmarshalValue(b, v)
			if err != nil {
				return err
			}
		} else {
			return errors.New(fmt.Sprint("Mapping component field ", c, " not found in passed struct of type ", v.Type().Name()))
		}
	}
	return nil
}

func (m *sparseMapping) extractComponents(column *Column, v *reflect.Value, bias int) ([][]byte, error) {
	var components [][]byte
	if len(m.components) > 0 {
		components = unpackComposite(column.Name)
	} else {
		components = [][]byte{column.Name}
	}
	if len(components) != (len(m.components) + bias) {
		return components, errors.New(fmt.Sprint("Returned number of components in composite column name does not match struct mapping in struct ", v.Type().Name()))
	}
	return components, nil
}

// TODO: speed this up
func (m *sparseMapping) isNewComponents(prev, next [][]byte, bias int) bool {
	if len(prev) != len(next) {
		return true
	}
	for i := 0; i < len(prev)-bias; i++ {
		p := prev[i]
		n := next[i]
		if len(p) != len(n) {
			return true
		}
		for j := 0; j < len(p); j++ {
			if p[j] != n[j] {
				return true
			}
		}
	}
	return false
}

func (m *sparseMapping) Unmap(destination interface{}, provider RowProvider) error {
	v, si, err := m.startUnmap(destination, provider)
	if err != nil {
		return err
	}

	compositeFieldsAreSet := false
	var previousComponents [][]byte

	for {
		column, err := provider.NextColumn()
		if err == Done {
			return Done
		} else if err == EndBeforeLimit {
			if compositeFieldsAreSet {
				break
			} else {
				return Done
			}
		} else if err == EndAtLimit {
			return Done
		} else if err != nil {
			return err
		}

		components, err := m.extractComponents(column, v, 1)
		if err != nil {
			return err
		}
		if !compositeFieldsAreSet {
			// first column
			if err := m.unmapComponents(v, si, components); err != nil {
				return err
			}
			compositeFieldsAreSet = true
		} else {
			if m.isNewComponents(previousComponents, components, 1) {
				provider.Rewind()
				break
			}
		}

		// lookup field by name
		var name string
		err = Unmarshal(components[len(components)-1], UTF8Type, &name)
		if err != nil {
			return errors.New(fmt.Sprint("Error unmarshaling composite field as UTF8Type for field name in struct ", v.Type().Name(), ", error: ", err))
		}
		if f, found := si.cassandraFields[name]; found {
			err := f.unmarshalValue(column.Value, v)
			if err != nil {
				return errors.New(fmt.Sprint("Error unmarshaling column: ", name, " value: ", err))
			}
		}

		previousComponents = components
	}

	return nil
}

func newCompactMapping(si *structInspection, cf string, keyField string, valueField string, componentFields ...string) Mapping {
	return &compactMapping{
		sparseMapping: *(newSparseMapping(si, cf, keyField, componentFields...).(*sparseMapping)),
		value:         valueField,
	}
}

type compactMapping struct {
	sparseMapping
	value string
}

func (m *compactMapping) Cf() string {
	return m.cf
}

func (m *compactMapping) Map(source interface{}) (*Row, error) {
	row, v, si, composite, err := m.startMap(source)
	if err != nil {
		return nil, err
	}
	if f, found := si.goFields[m.value]; found {
		columnValue, err := f.marshalValue(v)
		if err != nil {
			return nil, err
		}
		row.Columns = append(row.Columns, &Column{Name: composite, Value: columnValue})
	} else {
		row.Columns = append(row.Columns, &Column{Name: composite, Value: []byte{}})
	}
	return row, nil
}

func (m *compactMapping) Unmap(destination interface{}, provider RowProvider) error {
	v, si, err := m.startUnmap(destination, provider)
	if err != nil {
		return err
	}

	column, err := provider.NextColumn()
	if err == Done {
		return Done
	} else if err == EndBeforeLimit {
		return Done
	} else if err == EndAtLimit {
		return Done
	} else if err != nil {
		return err
	}

	components, err := m.extractComponents(column, v, 0)
	if err != nil {
		return err
	}
	if err := m.unmapComponents(v, si, components); err != nil {
		return err
	}
	if f, found := si.goFields[m.value]; found {
		err := f.unmarshalValue(column.Value, v)
		if err != nil {
			return errors.New(fmt.Sprint("Error unmarshaling column for compact value: ", err))
		}
	}

	return nil
}
