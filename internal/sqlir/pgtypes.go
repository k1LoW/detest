package sqlir

import "strings"

// postgresTypeAliases are the SQL names of pg_catalog's types a column is
// commonly declared with, as db/postgres's typeName gives them, which name no
// type of the catalog and so no array of one.
var postgresTypeAliases = map[string]bool{"boolean": true, "smallint": true, "integer": true, "int": true, "bigint": true, "real": true, "decimal": true}

// postgresCatalogTypes are the types of pg_catalog in Postgres 18, other than
// arrays and the catalogs' row types, which an unqualified name finds first.
var postgresCatalogTypes = func() map[string]bool {
	m := map[string]bool{}
	for t := range map[string]bool{
		"aclitem": true, "any": true, "anyarray": true, "anycompatible": true, "anycompatiblearray": true,
		"anycompatiblemultirange": true, "anycompatiblenonarray": true, "anycompatiblerange": true,
		"anyelement": true, "anyenum": true, "anymultirange": true, "anynonarray": true, "anyrange": true,
		"bit": true, "bool": true, "box": true, "bpchar": true, "bytea": true, "char": true, "cid": true,
		"cidr": true, "circle": true, "cstring": true, "date": true, "datemultirange": true, "daterange": true,
		"event_trigger": true, "fdw_handler": true, "float4": true, "float8": true, "gtsvector": true,
		"index_am_handler": true, "inet": true, "int2": true, "int2vector": true, "int4": true,
		"int4multirange": true, "int4range": true, "int8": true, "int8multirange": true, "int8range": true,
		"internal": true, "interval": true, "json": true, "jsonb": true, "jsonpath": true, "language_handler": true,
		"line": true, "lseg": true, "macaddr": true, "macaddr8": true, "money": true, "name": true, "numeric": true,
		"nummultirange": true, "numrange": true, "oid": true, "oidvector": true, "path": true, "point": true,
		"polygon": true, "record": true, "refcursor": true, "regclass": true, "regcollation": true,
		"regconfig": true, "regdictionary": true, "regnamespace": true, "regoper": true, "regoperator": true,
		"regproc": true, "regprocedure": true, "regrole": true, "regtype": true, "table_am_handler": true,
		"text": true, "tid": true, "time": true, "timestamp": true, "timestamptz": true, "timetz": true,
		"trigger": true, "tsm_handler": true, "tsmultirange": true, "tsquery": true, "tsrange": true,
		"tstzmultirange": true, "tstzrange": true, "tsvector": true, "txid_snapshot": true, "unknown": true,
		"uuid": true, "varbit": true, "varchar": true, "void": true, "xid": true, "xid8": true, "xml": true,
		"pg_brin_bloom_summary": true, "pg_brin_minmax_multi_summary": true, "pg_ddl_command": true,
		"pg_dependencies": true, "pg_lsn": true, "pg_mcv_list": true, "pg_ndistinct": true, "pg_node_tree": true,
		"pg_snapshot": true,
	} {
		m[t] = true
	}
	return m
}()

// postgresTypesWithoutArray are the postgresCatalogTypes Postgres 18 declares
// no array type of: the pseudo-types and the pg_ types of the planner's
// statistics.
var postgresTypesWithoutArray = map[string]bool{
	"any": true, "anyarray": true, "anycompatible": true, "anycompatiblearray": true, "anycompatiblemultirange": true,
	"anycompatiblenonarray": true, "anycompatiblerange": true, "anyelement": true, "anyenum": true,
	"anymultirange": true, "anynonarray": true, "anyrange": true, "event_trigger": true, "fdw_handler": true,
	"index_am_handler": true, "internal": true, "language_handler": true, "pg_brin_bloom_summary": true,
	"pg_brin_minmax_multi_summary": true, "pg_ddl_command": true, "pg_dependencies": true, "pg_mcv_list": true,
	"pg_ndistinct": true, "pg_node_tree": true, "table_am_handler": true, "trigger": true, "tsm_handler": true,
	"unknown": true, "void": true,
}

// PostgresBuiltinType reports whether name is a built-in type's, which a
// domain of the name would shadow by its bare name in detest but not in
// Postgres: one of postgresTypeAliases or postgresCatalogTypes, or the
// catalog name of an array of one of the latter, such as _int4.
func PostgresBuiltinType(name string) bool {
	if postgresTypeAliases[name] || postgresCatalogTypes[name] {
		return true
	}
	elem, ok := strings.CutPrefix(name, "_")
	return ok && postgresCatalogTypes[elem] && !postgresTypesWithoutArray[elem]
}
