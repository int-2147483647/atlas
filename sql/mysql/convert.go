// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package mysql

import (
	"fmt"
	"strconv"
	"strings"

	"ariga.io/atlas/sql/internal/sqlx"
	"ariga.io/atlas/sql/schema"
)

// FormatType converts schema type to its column form in the database.
// An error is returned if the type cannot be recognized.
func FormatType(t schema.Type) (string, error) {
	var f string
	switch t := t.(type) {
	case *BitType:
		f = strings.ToLower(t.T)
	case *schema.BoolType:
		// Map all flavors to a single form.
		switch f = strings.ToLower(t.T); f {
		case TypeBool, TypeBoolean, TypeTinyInt, "tinyint(1)":
			f = TypeBool
		}
	case *schema.BinaryType:
		f = strings.ToLower(t.T)
		if f == TypeVarBinary {
			// Zero is also a valid length.
			f = fmt.Sprintf("%s(%d)", f, t.Size)
		}
	case *schema.DecimalType:
		if f = strings.ToLower(t.T); f != TypeDecimal && f != TypeNumeric {
			return "", fmt.Errorf("mysql: unexpected decimal type: %q", t.T)
		}
		switch p, s := t.Precision, t.Scale; {
		case p < 0 || s < 0:
			return "", fmt.Errorf("mysql: decimal type must have precision > 0 and scale >= 0: %d, %d", p, s)
		case p < s:
			return "", fmt.Errorf("mysql: decimal type must have precision >= scale: %d < %d", p, s)
		case p == 0 && s == 0:
			// The default value for precision is 10 (i.e. decimal(0,0) = decimal(10)).
			p = 10
			fallthrough
		case s == 0:
			// In standard SQL, the syntax DECIMAL(M) is equivalent to DECIMAL(M,0),
			f = fmt.Sprintf("decimal(%d)", p)
		default:
			f = fmt.Sprintf("decimal(%d,%d)", p, s)
		}
	case *schema.EnumType:
		f = fmt.Sprintf("enum(%s)", formatValues(t.Values))
	case *schema.FloatType:
		f = strings.ToLower(t.T)
		// FLOAT with precision > 24, become DOUBLE.
		// Also, REAL is a synonym for DOUBLE (if REAL_AS_FLOAT was not set).
		if f == TypeFloat && t.Precision > 24 || f == TypeReal {
			f = TypeDouble
		}
	case *schema.IntegerType:
		f = strings.ToLower(t.T)
		if t.Unsigned {
			f += " unsigned"
		}
	case *schema.JSONType:
		f = strings.ToLower(t.T)
	case *SetType:
		f = fmt.Sprintf("enum(%s)", formatValues(t.Values))
	case *schema.StringType:
		f = strings.ToLower(t.T)
		switch f {
		case TypeChar:
			// Not a single char.
			if t.Size > 0 {
				f += fmt.Sprintf("(%d)", t.Size)
			}
		case TypeVarchar:
			// Zero is also a valid length.
			f = fmt.Sprintf("varchar(%d)", t.Size)
		}
	case *schema.SpatialType:
		f = strings.ToLower(t.T)
	case *schema.TimeType:
		f = strings.ToLower(t.T)
		if t.Precision > 0 {
			f = fmt.Sprintf("%s(%d)", f, t.Precision)
		}
	case *schema.UnsupportedType:
		// Do not accept unsupported types as we should cover all cases.
		return "", fmt.Errorf("unsupported type %q", t.T)
	default:
		return "", fmt.Errorf("invalid schema type %T", t)
	}
	return f, nil
}

// ParseType returns the schema.Type value represented by the given raw type.
// The raw value is expected to follow the format in MySQL information schema.
func ParseType(raw string) (schema.Type, error) {
	parts, size, unsigned, err := parseColumn(raw)
	if err != nil {
		return nil, err
	}
	switch t := parts[0]; t {
	case TypeBit:
		return &BitType{
			T: t,
		}, nil
	// bool and booleans are synonyms for
	// tinyint with display-width set to 1.
	case TypeBool, TypeBoolean:
		return &schema.BoolType{
			T: TypeBool,
		}, nil
	case TypeTinyInt, TypeSmallInt, TypeMediumInt, TypeInt, TypeBigInt:
		if size == 1 {
			return &schema.BoolType{
				T: TypeBool,
			}, nil
		}
		// For integer types, the size represents the display width and does not
		// constrain the range of values that can be stored in the column.
		// The storage byte-size is inferred from the type name (i.e TINYINT takes
		// a single byte).
		ft := &schema.IntegerType{
			T:        t,
			Unsigned: unsigned,
		}
		if attr := parts[len(parts)-1]; attr == "zerofill" && size != 0 {
			ft.Attrs = []schema.Attr{
				&DisplayWidth{
					N: int(size),
				},
				&ZeroFill{
					A: attr,
				},
			}
		}
		return ft, nil
	case TypeNumeric, TypeDecimal:
		dt := &schema.DecimalType{
			T: t,
		}
		if len(parts) > 1 {
			p, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse precision %q", parts[1])
			}
			dt.Precision = int(p)
		}
		if len(parts) > 2 {
			s, err := strconv.ParseInt(parts[2], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse scale %q", parts[1])
			}
			dt.Scale = int(s)
		}
		return dt, nil
	case TypeFloat, TypeDouble, TypeReal:
		ft := &schema.FloatType{
			T: t,
		}
		if len(parts) > 1 {
			p, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse precision %q", parts[1])
			}
			ft.Precision = int(p)
		}
		return ft, nil
	case TypeBinary, TypeVarBinary:
		return &schema.BinaryType{
			T:    t,
			Size: int(size),
		}, nil
	case TypeTinyBlob, TypeMediumBlob, TypeBlob, TypeLongBlob:
		return &schema.BinaryType{
			T: t,
		}, nil
	case TypeChar, TypeVarchar:
		return &schema.StringType{
			T:    t,
			Size: int(size),
		}, nil
	case TypeTinyText, TypeMediumText, TypeText, TypeLongText:
		return &schema.StringType{
			T: t,
		}, nil
	case TypeEnum, TypeSet:
		// Parse the enum values according to the MySQL format.
		// github.com/mysql/mysql-server/blob/8.0/sql/field.cc#Field_enum::sql_type
		rv := strings.TrimSuffix(strings.TrimPrefix(raw, t+"("), ")")
		if rv == "" {
			return nil, fmt.Errorf("mysql: unexpected enum type: %q", raw)
		}
		values := strings.Split(rv, "','")
		for i := range values {
			values[i] = strings.Trim(values[i], "'")
		}
		if t == TypeEnum {
			return &schema.EnumType{
				T:      TypeEnum,
				Values: values,
			}, nil
		}
		return &SetType{
			Values: values,
		}, nil
	case TypeDate, TypeDateTime, TypeTime, TypeTimestamp, TypeYear:
		tt := &schema.TimeType{
			T: t,
		}
		if len(parts) > 1 {
			p, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parse precision %q", parts[1])
			}
			tt.Precision = int(p)
		}
		return tt, nil
	case TypeJSON:
		return &schema.JSONType{
			T: t,
		}, nil
	case TypePoint, TypeMultiPoint, TypeLineString, TypeMultiLineString, TypePolygon, TypeMultiPolygon, TypeGeometry, TypeGeoCollection, TypeGeometryCollection:
		return &schema.SpatialType{
			T: t,
		}, nil
	default:
		return &schema.UnsupportedType{
			T: t,
		}, nil
	}
}

// mustFormat calls to FormatType and panics in case of error.
func mustFormat(t schema.Type) string {
	s, err := FormatType(t)
	if err != nil {
		panic(err)
	}
	return s
}

// formatValues formats ENUM and SET values.
func formatValues(vs []string) string {
	values := make([]string, len(vs))
	for i := range vs {
		values[i] = vs[i]
		if !sqlx.IsQuoted(values[i], '"', '\'') {
			values[i] = "'" + values[i] + "'"
		}
	}
	return strings.Join(values, ",")
}
