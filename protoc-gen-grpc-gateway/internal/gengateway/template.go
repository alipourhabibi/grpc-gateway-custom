package gengateway

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"text/template"

	"github.com/grpc-ecosystem/grpc-gateway/v2/internal/casing"
	"github.com/grpc-ecosystem/grpc-gateway/v2/internal/descriptor"
	"github.com/grpc-ecosystem/grpc-gateway/v2/utilities"
	"google.golang.org/grpc/grpclog"
)

type param struct {
	*descriptor.File
	Imports            []descriptor.GoPackage
	UseRequestContext  bool
	RegisterFuncSuffix string
	AllowPatchFeature  bool
	OmitPackageDoc     bool
}

type binding struct {
	*descriptor.Binding
	Registry          *descriptor.Registry
	AllowPatchFeature bool
}

// GetBodyFieldPath returns the binding body's field path.
func (b binding) GetBodyFieldPath() string {
	if b.Body != nil && len(b.Body.FieldPath) != 0 {
		return b.Body.FieldPath.String()
	}
	return "*"
}

// GetBodyFieldStructName returns the binding body's struct field name.
func (b binding) GetBodyFieldStructName() (string, error) {
	if b.Body != nil && len(b.Body.FieldPath) != 0 {
		return casing.Camel(b.Body.FieldPath.String()), nil
	}
	return "", errors.New("no body field found")
}

// HasQueryParam determines if the binding needs parameters in query string.
//
// It sometimes returns true even though actually the binding does not need.
// But it is not serious because it just results in a small amount of extra codes generated.
func (b binding) HasQueryParam() bool {
	return false
	if b.Body != nil && len(b.Body.FieldPath) == 0 {
		return false
	}
	fields := make(map[string]bool)
	for _, f := range b.Method.RequestType.Fields {
		fields[f.GetName()] = true
	}
	if b.Body != nil {
		delete(fields, b.Body.FieldPath.String())
	}
	for _, p := range b.PathParams {
		delete(fields, p.FieldPath.String())
	}
	return len(fields) > 0
}

func (b binding) QueryParamFilter() queryParamFilter {
	var seqs [][]string
	if b.Body != nil {
		seqs = append(seqs, strings.Split(b.Body.FieldPath.String(), "."))
		for _, comp := range b.Body.FieldPath {
			if comp.Target.JsonName != nil {
				seqs = append(seqs, strings.Split(*comp.Target.JsonName, "."))
			}
		}
	}
	for _, p := range b.PathParams {
		seqs = append(seqs, strings.Split(p.FieldPath.String(), "."))
		if p.Target.JsonName != nil {
			seqs = append(seqs, strings.Split(*p.Target.JsonName, "."))
		}
	}
	return queryParamFilter{utilities.NewDoubleArray(seqs)}
}

// HasEnumPathParam returns true if the path parameter slice contains a parameter
// that maps to an enum proto field that is not repeated, if not false is returned.
func (b binding) HasEnumPathParam() bool {
	return b.hasEnumPathParam(false)
}

// HasRepeatedEnumPathParam returns true if the path parameter slice contains a parameter
// that maps to a repeated enum proto field, if not false is returned.
func (b binding) HasRepeatedEnumPathParam() bool {
	return b.hasEnumPathParam(true)
}

// hasEnumPathParam returns true if the path parameter slice contains a parameter
// that maps to a enum proto field and that the enum proto field is or isn't repeated
// based on the provided 'repeated' parameter.
func (b binding) hasEnumPathParam(repeated bool) bool {
	for _, p := range b.PathParams {
		if p.IsEnum() && p.IsRepeated() == repeated {
			return true
		}
	}
	return false
}

// LookupEnum looks up a enum type by path parameter.
func (b binding) LookupEnum(p descriptor.Parameter) *descriptor.Enum {
	e, err := b.Registry.LookupEnum("", p.Target.GetTypeName())
	if err != nil {
		return nil
	}
	return e
}

// FieldMaskField returns the golang-style name of the variable for a FieldMask, if there is exactly one of that type in
// the message. Otherwise, it returns an empty string.
func (b binding) FieldMaskField() string {
	var fieldMaskField *descriptor.Field
	for _, f := range b.Method.RequestType.Fields {
		if f.GetTypeName() == ".google.protobuf.FieldMask" {
			// if there is more than 1 FieldMask for this request, then return none
			if fieldMaskField != nil {
				return ""
			}
			fieldMaskField = f
		}
	}
	if fieldMaskField != nil {
		return casing.Camel(fieldMaskField.GetName())
	}
	return ""
}

// queryParamFilter is a wrapper of utilities.DoubleArray which provides String() to output DoubleArray.Encoding in a stable and predictable format.
type queryParamFilter struct {
	*utilities.DoubleArray
}

func (f queryParamFilter) String() string {
	encodings := make([]string, len(f.Encoding))
	for str, enc := range f.Encoding {
		encodings[enc] = fmt.Sprintf("%q: %d", str, enc)
	}
	e := strings.Join(encodings, ", ")
	return fmt.Sprintf("&utilities.DoubleArray{Encoding: map[string]int{%s}, Base: %#v, Check: %#v}", e, f.Base, f.Check)
}

type trailerParams struct {
	Services           []*descriptor.Service
	UseRequestContext  bool
	RegisterFuncSuffix string
}

func applyTemplate(p param, reg *descriptor.Registry) (string, error) {
	w := bytes.NewBuffer(nil)
	if err := headerTemplate.Execute(w, p); err != nil {
		return "", err
	}
	var targetServices []*descriptor.Service

	for _, msg := range p.Messages {
		msgName := casing.Camel(*msg.Name)
		msg.Name = &msgName
	}

	for _, svc := range p.Services {
		var methodWithBindingsSeen bool
		svcName := casing.Camel(*svc.Name)
		svc.Name = &svcName

		for _, meth := range svc.Methods {
			if grpclog.V(2) {
				grpclog.Infof("Processing %s.%s", svc.GetName(), meth.GetName())
			}
			methName := casing.Camel(*meth.Name)
			meth.Name = &methName
			for _, b := range meth.Bindings {
				if err := reg.CheckDuplicateAnnotation(b.HTTPMethod, b.PathTmpl.Template, svc); err != nil {
					return "", err
				}

				methodWithBindingsSeen = true
				if err := handlerTemplate.Execute(w, binding{
					Binding:           b,
					Registry:          reg,
					AllowPatchFeature: p.AllowPatchFeature,
				}); err != nil {
					return "", err
				}

				// Local
				if err := localHandlerTemplate.Execute(w, binding{
					Binding:           b,
					Registry:          reg,
					AllowPatchFeature: p.AllowPatchFeature,
				}); err != nil {
					return "", err
				}
			}
		}
		if methodWithBindingsSeen {
			targetServices = append(targetServices, svc)
		}
	}
	if len(targetServices) == 0 {
		return "", errNoTargetService
	}

	tp := trailerParams{
		Services:           targetServices,
		UseRequestContext:  p.UseRequestContext,
		RegisterFuncSuffix: p.RegisterFuncSuffix,
	}
	// Local
	if err := localTrailerTemplate.Execute(w, tp); err != nil {
		return "", err
	}

	if err := trailerTemplate.Execute(w, tp); err != nil {
		return "", err
	}
	return w.String(), nil
}

var (
	headerTemplate = template.Must(template.New("header").Parse(`
// Code generated by protoc-gen-grpc-gateway. DO NOT EDIT.
// source: {{.GetName}}

{{if not .OmitPackageDoc}}/*
Package {{.GoPkg.Name}} is a reverse proxy.

It translates gRPC into RESTful JSON APIs.
*/{{end}}
package {{.GoPkg.Name}}
import (
	"encoding/json"
	{{range $i := .Imports}}{{if $i.Standard}}{{$i | printf "%s\n"}}{{end}}{{end}}

	{{range $i := .Imports}}{{if not $i.Standard}}{{$i | printf "%s\n"}}{{end}}{{end}}
)

var Acl authorization.AuthorizationServiceClient
var Authentication authentication.AuthenticationServiceClient

func InitAcl(acl authorization.AuthorizationServiceClient, auth authentication.AuthenticationServiceClient) {
	Acl = acl
	Authentication = auth
}
// Suppress "imported and not used" errors
var _ codes.Code
var _ io.Reader
var _ status.Status
var _ = runtime.String
var _ = utilities.NewDoubleArray
var _ = metadata.Join
`))

	handlerTemplate = template.Must(template.New("handler").Parse(`
{{if and .Method.GetClientStreaming .Method.GetServerStreaming}}
{{template "bidi-streaming-request-func" .}}
{{else if .Method.GetClientStreaming}}
{{template "client-streaming-request-func" .}}
{{else}}
{{template "client-rpc-request-func" .}}
{{end}}
`))

	_ = template.Must(handlerTemplate.New("request-func-signature").Parse(strings.ReplaceAll(`
{{if .Method.GetServerStreaming}}
func request_{{.Method.Service.GetName}}_{{.Method.GetName}}_{{.Index}}(ctx context.Context, marshaler runtime.Marshaler, client {{.Method.Service.InstanceName}}Client, req *http.Request, pathParams map[string]string) ({{.Method.Service.InstanceName}}_{{.Method.GetName}}Client, runtime.ServerMetadata, error)
{{else}}
func request_{{.Method.Service.GetName}}_{{.Method.GetName}}_{{.Index}}(ctx context.Context, marshaler runtime.Marshaler, client {{.Method.Service.InstanceName}}Client, req *http.Request, pathParams map[string]string) (proto.Message, runtime.ServerMetadata, error)
{{end}}`, "\n", "")))

	_ = template.Must(handlerTemplate.New("client-streaming-request-func").Parse(`
{{template "request-func-signature" .}} {
	var metadata runtime.ServerMetadata
	stream, err := client.{{.Method.GetName}}(ctx)
	if err != nil {
		grpclog.Infof("Failed to start streaming: %v", err)
		return nil, metadata, err
	}
	dec := marshaler.NewDecoder(req.Body)
	for {
		var protoReq {{.Method.RequestType.GoType .Method.Service.File.GoPkg.Path}}
		err = dec.Decode(&protoReq)
		if err == io.EOF {
			break
		}
		if err != nil {
			grpclog.Infof("Failed to decode request: %v", err)
			return nil, metadata, status.Errorf(codes.InvalidArgument, "%v", err)
		}
		if err = stream.Send(&protoReq); err != nil {
			if err == io.EOF {
				break
			}
			grpclog.Infof("Failed to send request: %v", err)
			return nil, metadata, err
		}
	}

	if err := stream.CloseSend(); err != nil {
		grpclog.Infof("Failed to terminate client stream: %v", err)
		return nil, metadata, err
	}
	header, err := stream.Header()
	if err != nil {
		grpclog.Infof("Failed to get header from client: %v", err)
		return nil, metadata, err
	}
	metadata.HeaderMD = header
{{if .Method.GetServerStreaming}}
	return stream, metadata, nil
{{else}}
	msg, err := stream.CloseAndRecv()
	metadata.TrailerMD = stream.Trailer()
	return msg, metadata, err
{{end}}
}
`))

	funcMap template.FuncMap = map[string]interface{}{
		"camelIdentifier": casing.CamelIdentifier,
		"Deref":           func(i *string) string { return *i },
		"CustomCase": func(i string) string {
			if len(i) > 0 {
				i = i[1:]
			}
			i = strings.ReplaceAll(i, ".", "_")
			return i
		},
	}

	_ = template.Must(handlerTemplate.New("client-rpc-request-func").Funcs(funcMap).Parse(`
{{$AllowPatchFeature := .AllowPatchFeature}}
{{if .HasQueryParam}}
var (
	filter_{{.Method.Service.GetName}}_{{.Method.GetName}}_{{.Index}} = {{.QueryParamFilter}}
)
{{end}}
{{template "request-func-signature" .}} {
	var t []byte
	var metadata runtime.ServerMetadata
	token, err := utils.GetTokenFromBearer(req.Header.Get("Authorization"))
	if err != nil {
		return nil, metadata, err
	}
	requ := &authorization.SanitizeRequest{
		Request: &authorization.Request{
			Query: new(string),
			Body: new(string),
		},
	}

	requ.AccessToken = token
	requ.Action = "read"
	requ.Object = "logistic_transport_vehicle"
	// TODO
	// *requ.Request.Query = "{}"
	*requ.Request.Query = req.URL.Query().Encode()
	requ.Scope = []string{
		"whole",
	}
	{{ $bodyentity := "" }}
	{{ $hasFilter := false }}
	protoReq := {{.Method.RequestType.GoType .Method.Service.File.GoPkg.Path}}{
	{{ range .Binding.Method.RequestType.Fields }}
		{{ if eq (Deref .Name) "body" }}
		{{ $bodyentity = .JsonName }}
		Body: &{{ .JsonName }}{},
		{{ end }}
		{{ if eq (Deref .Name) "filter" }}
		{{ $hasFilter = true }}
		Filter: &{{ .JsonName }}{},
		{{ end }}
		{{ if eq (Deref .Name) "operations" }}
		Operations: &{{ .JsonName }}{},
		{{ end }}
	{{ end }}
	}
{{if .Body}}
	newReader, berr := utilities.IOReaderFactory(req.Body)
	if berr != nil {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "%v", berr)
	}
	{{- $protoReq := .Body.AssignableExprPrep "protoReq" .Method.Service.File.GoPkg.Path -}}
	{{- if ne "" $protoReq }}
	{{printf "%s" $protoReq }}
	{{- end}}
	if err := marshaler.NewDecoder(newReader()).Decode(&{{.Body.AssignableExpr "protoReq" .Method.Service.File.GoPkg.Path}}); err != nil && err != io.EOF  {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	t, err = json.Marshal(protoReq.Body)
	if err != nil {
		return nil, metadata, err
	}


	*requ.Request.Body = string(t)


	{{- if and $AllowPatchFeature (eq (.HTTPMethod) "PATCH") (.FieldMaskField) (not (eq "*" .GetBodyFieldPath)) }}
	if protoReq.{{.FieldMaskField}} == nil || len(protoReq.{{.FieldMaskField}}.GetPaths()) == 0 {
			if fieldMask, err := runtime.FieldMaskFromRequestBody(newReader(), protoReq.{{.GetBodyFieldStructName}}); err != nil {
				return nil, metadata, status.Errorf(codes.InvalidArgument, "%v", err)
			} else {
				protoReq.{{.FieldMaskField}} = fieldMask
			}
	}
	{{end}}
{{end}}
{{if .PathParams}}
	var (
		val string
{{- if .HasEnumPathParam}}
		e int32
{{- end}}
{{- if .HasRepeatedEnumPathParam}}
		es []int32
{{- end}}
		ok bool
		err error
		_ = err
	)
	{{$binding := .}}
	{{range $param := .PathParams}}
	{{$enum := $binding.LookupEnum $param}}
	val, ok = pathParams[{{$param | printf "%q"}}]
	if !ok {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "missing parameter %s", {{$param | printf "%q"}})
	}
{{if $param.IsNestedProto3}}
	err = runtime.PopulateFieldFromPath(&protoReq, {{$param | printf "%q"}}, val)
	if err != nil {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "type mismatch, parameter: %s, error: %v", {{$param | printf "%q"}}, err)
	}
	{{if $enum}}
		e{{if $param.IsRepeated}}s{{end}}, err = {{$param.ConvertFuncExpr}}(val{{if $param.IsRepeated}}, {{$binding.Registry.GetRepeatedPathParamSeparator | printf "%c" | printf "%q"}}{{end}}, {{$enum.GoType $param.Method.Service.File.GoPkg.Path | camelIdentifier}}_value)
		if err != nil {
			return nil, metadata, status.Errorf(codes.InvalidArgument, "could not parse path as enum value, parameter: %s, error: %v", {{$param | printf "%q"}}, err)
		}
	{{end}}
{{else if $enum}}
	e{{if $param.IsRepeated}}s{{end}}, err = {{$param.ConvertFuncExpr}}(val{{if $param.IsRepeated}}, {{$binding.Registry.GetRepeatedPathParamSeparator | printf "%c" | printf "%q"}}{{end}}, {{$enum.GoType $param.Method.Service.File.GoPkg.Path | camelIdentifier}}_value)
	if err != nil {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "type mismatch, parameter: %s, error: %v", {{$param | printf "%q"}}, err)
	}
{{else}}
	{{- $protoReq := $param.AssignableExprPrep "protoReq" $binding.Method.Service.File.GoPkg.Path -}}
	{{- if ne "" $protoReq }}
	{{printf "%s" $protoReq }}
	{{- end}}
	{{$param.AssignableExpr "protoReq" $binding.Method.Service.File.GoPkg.Path}}, err = {{$param.ConvertFuncExpr}}(val{{if $param.IsRepeated}}, {{$binding.Registry.GetRepeatedPathParamSeparator | printf "%c" | printf "%q"}}{{end}})
	if err != nil {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "type mismatch, parameter: %s, error: %v", {{$param | printf "%q"}}, err)
	}
{{end}}
{{if and $enum $param.IsRepeated}}
	s := make([]{{$enum.GoType $param.Method.Service.File.GoPkg.Path}}, len(es))
	for i, v := range es {
		s[i] = {{$enum.GoType $param.Method.Service.File.GoPkg.Path}}(v)
	}
	{{$param.AssignableExpr "protoReq" $binding.Method.Service.File.GoPkg.Path}} = s
{{else if $enum}}
	{{$param.AssignableExpr "protoReq" $binding.Method.Service.File.GoPkg.Path}} = {{$enum.GoType $param.Method.Service.File.GoPkg.Path | camelIdentifier}}(e)
{{end}}
	{{end}}
{{end}}
	/*
		queries := req.URL.Query().Encode()
		tok, err := Auth.GetTokenFromBearer(r.Header.Get("Authorization"))
		if err != nil {
			res = append(res, err)
		}
	*/
{{if .Method.GetServerStreaming}}
	stream, err := client.{{.Method.GetName}}(ctx, &protoReq)
	if err != nil {
		return nil, metadata, err
	}
	header, err := stream.Header()
	if err != nil {
		return nil, metadata, err
	}
	metadata.HeaderMD = header
	return stream, metadata, nil
{{else}}
	newReq, err := Acl.Sanitize(ctx, requ)
	if err != nil {
		return nil, metadata, err
	}
{{ if $hasFilter }}
	newQuery, err := utils.MetadataQuery(*newReq.Request.Query)
	if err != nil {
		return nil, metadata, err
	}
	protoReq.Filter.Query = newQuery
{{ end }}

{{ if .Body }}
	if newReq.Request.Body != nil {
		t, err = json.Marshal(newReq.Request.Body)
		if err != nil {
			return nil, metadata, err
		}
		err = json.Unmarshal(t, protoReq.Body)
		if err != nil {
			return nil, metadata, err
		}
	}
{{ end }}
	data, err := client.{{.Method.GetName}}(ctx, &protoReq, grpc.Header(&metadata.HeaderMD), grpc.Trailer(&metadata.TrailerMD))
	if err != nil {
		return nil, metadata, err
	}

	decodedToken, err := Authentication.Decrypt(ctx, &authentication.AccessToken{
		AccessToken: token,
	})
	items := []string{}

	item, err := json.Marshal(data)
	if err != nil {
		return nil, metadata, err
	}
	items = append(items, string(item))

	req2, err := utils.FilterAll("read", decodedToken, newReq.Grants, items)
	if err != nil {
		return nil, metadata, err
	}
	t, err = json.Marshal(req2)
	if err != nil {
		return nil, metadata, err
	}
	response := {{ .Binding.Method.ResponseType.Name }}{}
	err = json.Unmarshal(t, &response)
	if err != nil {
	     return nil, metadata, err
	}
	return &response, metadata, err
{{end}}
}`))

	_ = template.Must(handlerTemplate.New("bidi-streaming-request-func").Parse(`
{{template "request-func-signature" .}} {
	var metadata runtime.ServerMetadata
	stream, err := client.{{.Method.GetName}}(ctx)
	if err != nil {
		grpclog.Infof("Failed to start streaming: %v", err)
		return nil, metadata, err
	}
	dec := marshaler.NewDecoder(req.Body)
	handleSend := func() error {
		var protoReq {{.Method.RequestType.GoType .Method.Service.File.GoPkg.Path}}
		err := dec.Decode(&protoReq)
		if err == io.EOF {
			return err
		}
		if err != nil {
			grpclog.Infof("Failed to decode request: %v", err)
			return err
		}
		if err := stream.Send(&protoReq); err != nil {
			grpclog.Infof("Failed to send request: %v", err)
			return err
		}
		return nil
	}
	go func() {
		for {
			if err := handleSend(); err != nil {
				break
			}
		}
		if err := stream.CloseSend(); err != nil {
			grpclog.Infof("Failed to terminate client stream: %v", err)
		}
	}()
	header, err := stream.Header()
	if err != nil {
		grpclog.Infof("Failed to get header from client: %v", err)
		return nil, metadata, err
	}
	metadata.HeaderMD = header
	return stream, metadata, nil
}
`))

	localHandlerTemplate = template.Must(template.New("local-handler").Parse(`
{{if and .Method.GetClientStreaming .Method.GetServerStreaming}}
{{else if .Method.GetClientStreaming}}
{{else if .Method.GetServerStreaming}}
{{else}}
{{template "local-client-rpc-request-func" .}}
{{end}}
`))

	_ = template.Must(localHandlerTemplate.New("local-request-func-signature").Parse(strings.ReplaceAll(`
{{if .Method.GetServerStreaming}}
{{else}}
func local_request_{{.Method.Service.GetName}}_{{.Method.GetName}}_{{.Index}}(ctx context.Context, marshaler runtime.Marshaler, server {{.Method.Service.InstanceName}}Server, req *http.Request, pathParams map[string]string) (proto.Message, runtime.ServerMetadata, error)
{{end}}`, "\n", "")))

	_ = template.Must(localHandlerTemplate.New("local-client-rpc-request-func").Funcs(funcMap).Parse(`
{{$AllowPatchFeature := .AllowPatchFeature}}
{{template "local-request-func-signature" .}} {
	var protoReq {{.Method.RequestType.GoType .Method.Service.File.GoPkg.Path}}
	var metadata runtime.ServerMetadata
{{if .Body}}
	newReader, berr := utilities.IOReaderFactory(req.Body)
	if berr != nil {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "%v", berr)
	}
	{{- $protoReq := .Body.AssignableExprPrep "protoReq" .Method.Service.File.GoPkg.Path -}}
	{{- if ne "" $protoReq }}
	{{printf "%s" $protoReq }}
	{{- end}}
	if err := marshaler.NewDecoder(newReader()).Decode(&{{.Body.AssignableExpr "protoReq" .Method.Service.File.GoPkg.Path}}); err != nil && err != io.EOF  {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	{{- if and $AllowPatchFeature (eq (.HTTPMethod) "PATCH") (.FieldMaskField) (not (eq "*" .GetBodyFieldPath)) }}
	if protoReq.{{.FieldMaskField}} == nil || len(protoReq.{{.FieldMaskField}}.GetPaths()) == 0 {
			if fieldMask, err := runtime.FieldMaskFromRequestBody(newReader(), protoReq.{{.GetBodyFieldStructName}}); err != nil {
				return nil, metadata, status.Errorf(codes.InvalidArgument, "%v", err)
			} else {
				protoReq.{{.FieldMaskField}} = fieldMask
			}
	}
	{{end}}
{{end}}
{{if .PathParams}}
	var (
		val string
{{- if .HasEnumPathParam}}
		e int32
{{- end}}
{{- if .HasRepeatedEnumPathParam}}
		es []int32
{{- end}}
		ok bool
		err error
		_ = err
	)
	{{$binding := .}}
	{{range $param := .PathParams}}
	{{$enum := $binding.LookupEnum $param}}
	val, ok = pathParams[{{$param | printf "%q"}}]
	if !ok {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "missing parameter %s", {{$param | printf "%q"}})
	}
{{if $param.IsNestedProto3}}
	err = runtime.PopulateFieldFromPath(&protoReq, {{$param | printf "%q"}}, val)
	if err != nil {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "type mismatch, parameter: %s, error: %v", {{$param | printf "%q"}}, err)
	}
	{{if $enum}}
		e{{if $param.IsRepeated}}s{{end}}, err = {{$param.ConvertFuncExpr}}(val{{if $param.IsRepeated}}, {{$binding.Registry.GetRepeatedPathParamSeparator | printf "%c" | printf "%q"}}{{end}}, {{$enum.GoType $param.Method.Service.File.GoPkg.Path | camelIdentifier}}_value)
		if err != nil {
			return nil, metadata, status.Errorf(codes.InvalidArgument, "could not parse path as enum value, parameter: %s, error: %v", {{$param | printf "%q"}}, err)
		}
	{{end}}
{{else if $enum}}
	e{{if $param.IsRepeated}}s{{end}}, err = {{$param.ConvertFuncExpr}}(val{{if $param.IsRepeated}}, {{$binding.Registry.GetRepeatedPathParamSeparator | printf "%c" | printf "%q"}}{{end}}, {{$enum.GoType  $param.Method.Service.File.GoPkg.Path | camelIdentifier}}_value)
	if err != nil {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "type mismatch, parameter: %s, error: %v", {{$param | printf "%q"}}, err)
	}
{{else}}
	{{- $protoReq := $param.AssignableExprPrep "protoReq" $binding.Method.Service.File.GoPkg.Path -}}
	{{- if ne "" $protoReq }}
	{{printf "%s" $protoReq }}
	{{- end}}
	{{$param.AssignableExpr "protoReq" $binding.Method.Service.File.GoPkg.Path}}, err = {{$param.ConvertFuncExpr}}(val{{if $param.IsRepeated}}, {{$binding.Registry.GetRepeatedPathParamSeparator | printf "%c" | printf "%q"}}{{end}})
	if err != nil {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "type mismatch, parameter: %s, error: %v", {{$param | printf "%q"}}, err)
	}
{{end}}

{{if and $enum $param.IsRepeated}}
	s := make([]{{$enum.GoType $param.Method.Service.File.GoPkg.Path}}, len(es))
	for i, v := range es {
		s[i] = {{$enum.GoType $param.Method.Service.File.GoPkg.Path}}(v)
	}
	{{$param.AssignableExpr "protoReq" $binding.Method.Service.File.GoPkg.Path}} = s
{{else if $enum}}
	{{$param.AssignableExpr "protoReq" $binding.Method.Service.File.GoPkg.Path}} = {{$enum.GoType $param.Method.Service.File.GoPkg.Path | camelIdentifier}}(e)
{{end}}
	{{end}}
{{end}}
{{if .HasQueryParam}}
	if err := req.ParseForm(); err != nil {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	if err := runtime.PopulateQueryParameters(&protoReq, req.Form, filter_{{.Method.Service.GetName}}_{{.Method.GetName}}_{{.Index}}); err != nil {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "%v", err)
	}
{{end}}
{{if .Method.GetServerStreaming}}
	// TODO
{{else}}
	msg, err := server.{{.Method.GetName}}(ctx, &protoReq)
	return msg, metadata, err
{{end}}
}`))

	localTrailerTemplate = template.Must(template.New("local-trailer").Parse(`
{{$UseRequestContext := .UseRequestContext}}
{{range $svc := .Services}}
// Register{{$svc.GetName}}{{$.RegisterFuncSuffix}}Server registers the http handlers for service {{$svc.GetName}} to "mux".
// UnaryRPC     :call {{$svc.GetName}}Server directly.
// StreamingRPC :currently unsupported pending https://github.com/grpc/grpc-go/issues/906.
// Note that using this registration option will cause many gRPC library features to stop working. Consider using Register{{$svc.GetName}}{{$.RegisterFuncSuffix}}FromEndpoint instead.
func Register{{$svc.GetName}}{{$.RegisterFuncSuffix}}Server(ctx context.Context, mux *runtime.ServeMux, server {{$svc.InstanceName}}Server) error {
	{{range $m := $svc.Methods}}
	{{range $b := $m.Bindings}}
	{{if or $m.GetClientStreaming $m.GetServerStreaming}}
	mux.Handle({{$b.HTTPMethod | printf "%q"}}, pattern_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}, func(w http.ResponseWriter, req *http.Request, pathParams map[string]string) {
		err := status.Error(codes.Unimplemented, "streaming calls are not yet supported in the in-process transport")
		_, outboundMarshaler := runtime.MarshalerForRequest(mux, req)
		runtime.HTTPError(ctx, mux, outboundMarshaler, w, req, err)
		return
	})
	{{else}}
	mux.Handle({{$b.HTTPMethod | printf "%q"}}, pattern_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}, func(w http.ResponseWriter, req *http.Request, pathParams map[string]string) {
	{{- if $UseRequestContext }}
		ctx, cancel := context.WithCancel(req.Context())
	{{- else -}}
		ctx, cancel := context.WithCancel(ctx)
	{{- end }}
		defer cancel()
		var stream runtime.ServerTransportStream
		ctx = grpc.NewContextWithServerTransportStream(ctx, &stream)
		inboundMarshaler, outboundMarshaler := runtime.MarshalerForRequest(mux, req)
		var err error
		var annotatedContext context.Context
		{{- if $b.PathTmpl }}
		annotatedContext, err = runtime.AnnotateIncomingContext(ctx, mux, req, "/{{$svc.File.GetPackage}}.{{$svc.GetName}}/{{$m.GetName}}", runtime.WithHTTPPathPattern("{{$b.PathTmpl.Template}}"))
		{{- else -}}
		annotatedContext, err = runtime.AnnotateIncomingContext(ctx, mux, req, "/{{$svc.File.GetPackage}}.{{$svc.GetName}}/{{$m.GetName}}")
		{{- end }}
		if err != nil {
			runtime.HTTPError(ctx, mux, outboundMarshaler, w, req, err)
			return
		}
		resp, md, err := local_request_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}(annotatedContext, inboundMarshaler, server, req, pathParams)
		md.HeaderMD, md.TrailerMD = metadata.Join(md.HeaderMD, stream.Header()), metadata.Join(md.TrailerMD, stream.Trailer())
		annotatedContext = runtime.NewServerMetadataContext(annotatedContext, md)
		if err != nil {
			runtime.HTTPError(annotatedContext, mux, outboundMarshaler, w, req, err)
			return
		}

		{{ if $b.ResponseBody }}
		forward_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}(annotatedContext, mux, outboundMarshaler, w, req, response_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}{resp}, mux.GetForwardResponseOptions()...)
		{{ else }}
		forward_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}(annotatedContext, mux, outboundMarshaler, w, req, resp, mux.GetForwardResponseOptions()...)
		{{end}}
	})
	{{end}}
	{{end}}
	{{end}}
	return nil
}
{{end}}`))

	trailerTemplate = template.Must(template.New("trailer").Parse(`
{{$UseRequestContext := .UseRequestContext}}
{{range $svc := .Services}}
// Register{{$svc.GetName}}{{$.RegisterFuncSuffix}}FromEndpoint is same as Register{{$svc.GetName}}{{$.RegisterFuncSuffix}} but
// automatically dials to "endpoint" and closes the connection when "ctx" gets done.
func Register{{$svc.GetName}}{{$.RegisterFuncSuffix}}FromEndpoint(ctx context.Context, mux *runtime.ServeMux, endpoint string, opts []grpc.DialOption) (err error) {
	conn, err := grpc.DialContext(ctx, endpoint, opts...)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if cerr := conn.Close(); cerr != nil {
				grpclog.Infof("Failed to close conn to %s: %v", endpoint, cerr)
			}
			return
		}
		go func() {
			<-ctx.Done()
			if cerr := conn.Close(); cerr != nil {
				grpclog.Infof("Failed to close conn to %s: %v", endpoint, cerr)
			}
		}()
	}()

	return Register{{$svc.GetName}}{{$.RegisterFuncSuffix}}(ctx, mux, conn)
}

// Register{{$svc.GetName}}{{$.RegisterFuncSuffix}} registers the http handlers for service {{$svc.GetName}} to "mux".
// The handlers forward requests to the grpc endpoint over "conn".
func Register{{$svc.GetName}}{{$.RegisterFuncSuffix}}(ctx context.Context, mux *runtime.ServeMux, conn *grpc.ClientConn) error {
	return Register{{$svc.GetName}}{{$.RegisterFuncSuffix}}Client(ctx, mux, {{$svc.ClientConstructorName}}(conn))
}

// Register{{$svc.GetName}}{{$.RegisterFuncSuffix}}Client registers the http handlers for service {{$svc.GetName}}
// to "mux". The handlers forward requests to the grpc endpoint over the given implementation of "{{$svc.InstanceName}}Client".
// Note: the gRPC framework executes interceptors within the gRPC handler. If the passed in "{{$svc.InstanceName}}Client"
// doesn't go through the normal gRPC flow (creating a gRPC client etc.) then it will be up to the passed in
// "{{$svc.InstanceName}}Client" to call the correct interceptors.
func Register{{$svc.GetName}}{{$.RegisterFuncSuffix}}Client(ctx context.Context, mux *runtime.ServeMux, client {{$svc.InstanceName}}Client) error {
	{{range $m := $svc.Methods}}
	{{range $b := $m.Bindings}}
	mux.Handle({{$b.HTTPMethod | printf "%q"}}, pattern_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}, func(w http.ResponseWriter, req *http.Request, pathParams map[string]string) {
	{{- if $UseRequestContext }}
		ctx, cancel := context.WithCancel(req.Context())
	{{- else -}}
		ctx, cancel := context.WithCancel(ctx)
	{{- end }}
		defer cancel()
		inboundMarshaler, outboundMarshaler := runtime.MarshalerForRequest(mux, req)
		var err error
		var annotatedContext context.Context
		{{- if $b.PathTmpl }}
		annotatedContext, err = runtime.AnnotateContext(ctx, mux, req, "/{{$svc.File.GetPackage}}.{{$svc.GetName}}/{{$m.GetName}}", runtime.WithHTTPPathPattern("{{$b.PathTmpl.Template}}"))
		{{- else -}}
		annotatedContext, err = runtime.AnnotateContext(ctx, mux, req, "/{{$svc.File.GetPackage}}.{{$svc.GetName}}/{{$m.GetName}}")
		{{- end }}
		if err != nil {
			runtime.HTTPError(ctx, mux, outboundMarshaler, w, req, err)
			return
		}
		resp, md, err := request_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}(annotatedContext, inboundMarshaler, client, req, pathParams)
		annotatedContext = runtime.NewServerMetadataContext(annotatedContext, md)
		if err != nil {
			runtime.HTTPError(annotatedContext, mux, outboundMarshaler, w, req, err)
			return
		}
		{{if $m.GetServerStreaming}}
		{{ if $b.ResponseBody }}
		forward_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}(annotatedContext, mux, outboundMarshaler, w, req, func() (proto.Message, error) {
			res, err := resp.Recv()
			return response_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}{res}, err
		}, mux.GetForwardResponseOptions()...)
		{{ else }}
		forward_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}(annotatedContext, mux, outboundMarshaler, w, req, func() (proto.Message, error) { return resp.Recv() }, mux.GetForwardResponseOptions()...)
		{{end}}
		{{else}}
		{{ if $b.ResponseBody }}
		forward_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}(annotatedContext, mux, outboundMarshaler, w, req, response_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}{resp}, mux.GetForwardResponseOptions()...)
		{{ else }}
		forward_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}(annotatedContext, mux, outboundMarshaler, w, req, resp, mux.GetForwardResponseOptions()...)
		{{end}}
		{{end}}
	})
	{{end}}
	{{end}}
	return nil
}

{{range $m := $svc.Methods}}
{{range $b := $m.Bindings}}
{{if $b.ResponseBody}}
type response_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}} struct {
	proto.Message
}

func (m response_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}) XXX_ResponseBody() interface{} {
	response := m.Message.(*{{$m.ResponseType.GoType $m.Service.File.GoPkg.Path}})
	return {{$b.ResponseBody.AssignableExpr "response" $m.Service.File.GoPkg.Path}}
}
{{end}}
{{end}}
{{end}}

var (
	{{range $m := $svc.Methods}}
	{{range $b := $m.Bindings}}
	pattern_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}} = runtime.MustPattern(runtime.NewPattern({{$b.PathTmpl.Version}}, {{$b.PathTmpl.OpCodes | printf "%#v"}}, {{$b.PathTmpl.Pool | printf "%#v"}}, {{$b.PathTmpl.Verb | printf "%q"}}))
	{{end}}
	{{end}}
)

var (
	{{range $m := $svc.Methods}}
	{{range $b := $m.Bindings}}
	forward_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}} = {{if $m.GetServerStreaming}}runtime.ForwardResponseStream{{else}}runtime.ForwardResponseMessage{{end}}
	{{end}}
	{{end}}
)
{{end}}`))

	aclTemplate = template.Must(template.New("acl").Parse(`
	requ := &auth_authorization.SanitizeRequest{
		Request: &auth_authorization.Request{
			Body:  &temp,
			Query: &temp,
		},
	}
`))
)
