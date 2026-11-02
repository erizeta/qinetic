package qinetic

import (
	"encoding/xml"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/erizeta/qilat"
)

// Bind decodes request data into a struct, returned by c.Bind. Malformed input fails with a 400 *Error
// whose message names at most the offending field; the decoder's own error is kept only for logs.
type Bind struct{ c *Ctx }

// Bind returns the binder for the request.
func (c *Ctx) Bind() *Bind {
	c.live()
	return &Bind{c: c}
}

// JSON decodes the body as JSON with Config.JSONDecoder.
func (b *Bind) JSON(v any) error {
	body, err := b.c.Body()
	if err != nil {
		return err
	}
	if err := b.c.app.jsonDecode(body, v); err != nil {
		return clientError(http.StatusBadRequest, "invalid JSON body", err)
	}
	return nil
}

// XML decodes the body as XML.
func (b *Bind) XML(v any) error {
	body, err := b.c.Body()
	if err != nil {
		return err
	}
	if err := xml.Unmarshal(body, v); err != nil {
		return clientError(http.StatusBadRequest, "invalid XML body", err)
	}
	return nil
}

// Form decodes an application/x-www-form-urlencoded or multipart/form-data body into the fields of
// the struct v points to, named by `form` tags or else by field name, matched case-insensitively as in
// Fiber. Supported field types are string, bool, integers, floats, and slices of them.
func (b *Bind) Form(v any) error {
	c := b.c
	if err := c.parseForm(); err != nil {
		return err
	}
	if c.mform != nil {
		values := make(map[string][]string, len(c.mform.Value))
		for k, vs := range c.mform.Value {
			key := strings.ToLower(k)
			values[key] = append(values[key], vs...)
		}
		return decodeValues(v, "form", values)
	}
	return decodeValues(v, "form", foldArgs(&c.form))
}

// Query decodes the query string into the struct v points to, like Form but with `query` tags.
func (b *Bind) Query(v any) error {
	return decodeValues(v, "query", foldArgs(b.c.r.URI().Query()))
}

// Body decodes the body according to its Content-Type: JSON, XML, or a form. Other types fail with
// ErrUnsupportedMediaType.
func (b *Bind) Body(v any) error {
	mediaType, _, err := mime.ParseMediaType(string(b.c.r.Header.Peek("Content-Type")))
	if err != nil {
		return ErrUnsupportedMediaType
	}
	switch {
	case mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"):
		return b.JSON(v)
	case mediaType == "application/xml" || mediaType == "text/xml" || strings.HasSuffix(mediaType, "+xml"):
		return b.XML(v)
	case mediaType == "application/x-www-form-urlencoded" || mediaType == "multipart/form-data":
		return b.Form(v)
	}
	return ErrUnsupportedMediaType
}

// foldArgs copies args into a map keyed by lower-case name, keeping repeated values in order.
func foldArgs(args *qilat.Args) map[string][]string {
	values := make(map[string][]string, args.Len())
	for k, v := range args.All() {
		key := strings.ToLower(string(k))
		values[key] = append(values[key], string(v))
	}
	return values
}

type bindField struct {
	index []int
	name  string // as declared, for error messages
	key   string // lower-case, for matching
}

type bindKey struct {
	t   reflect.Type
	tag string
}

var bindFields sync.Map // bindKey -> []bindField

func fieldsOf(t reflect.Type, tag string) ([]bindField, error) {
	key := bindKey{t, tag}
	if f, ok := bindFields.Load(key); ok {
		return f.([]bindField), nil
	}
	var fields []bindField
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(sf.Tag.Get(tag), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		if !bindable(sf.Type) {
			return nil, fmt.Errorf("qinetic: cannot bind field %s of type %s", sf.Name, sf.Type)
		}
		fields = append(fields, bindField{index: sf.Index, name: name, key: strings.ToLower(name)})
	}
	bindFields.Store(key, fields)
	return fields, nil
}

func bindable(t reflect.Type) bool {
	if t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// decodeValues sets each field of the struct v points to from values, keyed by lower-case name. Absent
// keys leave fields as they are. A v that is not a non-nil struct pointer is a programming error,
// reported as a plain error.
func decodeValues(v any, tag string, values map[string][]string) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("qinetic: bind target must be a non-nil pointer to a struct, not %T", v)
	}
	rv = rv.Elem()
	fields, err := fieldsOf(rv.Type(), tag)
	if err != nil {
		return err
	}
	for _, f := range fields {
		vals := values[f.key]
		if len(vals) == 0 {
			continue
		}
		fv := rv.FieldByIndex(f.index)
		if fv.Kind() == reflect.Slice {
			s := reflect.MakeSlice(fv.Type(), len(vals), len(vals))
			for i, val := range vals {
				if err := setScalar(s.Index(i), val); err != nil {
					return clientError(http.StatusBadRequest, fmt.Sprintf("invalid value for %q", f.name), err)
				}
			}
			fv.Set(s)
			continue
		}
		if err := setScalar(fv, vals[0]); err != nil {
			return clientError(http.StatusBadRequest, fmt.Sprintf("invalid value for %q", f.name), err)
		}
	}
	return nil
}

func setScalar(v reflect.Value, s string) error {
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		if s == "on" { // HTML checkboxes
			v.SetBool(true)
			return nil
		}
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(s, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetFloat(n)
	default:
		return errors.New("unsupported kind " + v.Kind().String())
	}
	return nil
}
