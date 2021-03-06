package gossie

import (
	"github.com/HailoOSS/gossie/src/cassandra"
)

/*
to do:
    generate CQL schema from tagged Go structs
    validate tagged Go structs against schemas
    handle ReversedType
    handle type options
	handle composited column names in the schema (is this in use/allowed?)
*/

type Schema struct {
	ColumnFamilies map[string]*ColumnFamily
}

type ColumnFamily struct {
	DefaultComparator TypeClass
	DefaultValidator  TypeClass
	KeyValidator      TypeClass
	NamedColumns      map[string]TypeClass
}

func newSchema(ksDef *cassandra.KsDef) *Schema {
	cfDefs := ksDef.CfDefs
	schema := &Schema{ColumnFamilies: make(map[string]*ColumnFamily)}

	for _, cfDef := range cfDefs {

		// FIXME: this is weird, but happens a lot. thrift4go problem?
		if cfDef == nil {
			continue
		}

		if *cfDef.ColumnType != "Standard" {
			continue
		}

		cf := &ColumnFamily{}

		cf.DefaultComparator = parseTypeClass(*cfDef.ComparatorType)
		cf.DefaultValidator = parseTypeClass(*cfDef.DefaultValidationClass)
		cf.KeyValidator = parseTypeClass(*cfDef.KeyValidationClass)

		cf.NamedColumns = make(map[string]TypeClass)

		for _, colDef := range *cfDef.ColumnMetadata {
			// FIXME: this is weird, but happens a lot. thrift4go problem?
			if colDef == nil {
				continue
			}
			name := string(colDef.Name[0:(len(colDef.Name))])
			cf.NamedColumns[name] = parseTypeClass(colDef.ValidationClass)
		}

		schema.ColumnFamilies[cfDef.Name] = cf
	}

	return schema
}
