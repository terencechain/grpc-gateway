package gengateway

import (
	"bytes"
	"errors"
	"fmt"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"log"
	"regexp"
	"strings"
	"text/template"

	"github.com/golang/glog"
	"github.com/grpc-ecosystem/grpc-gateway/v2/internal/casing"
	"github.com/grpc-ecosystem/grpc-gateway/v2/internal/descriptor"
	"github.com/grpc-ecosystem/grpc-gateway/v2/utilities"
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
	TypeFromName func(string) string
	DecodeFromHex func(string, string) bool
	JsonNameToGoName func(string) string
	FieldsToEncode func(string) []string
	EncodeOutputField func(string) string
	EncodingFieldPaths func(string) []string
}

// GetBodyFieldPath returns the binding body's fieldpath.
func (b binding) GetBodyFieldPath() string {
	if b.Body != nil && len(b.Body.FieldPath) != 0 {
		return b.Body.FieldPath.String()
	}
	return "*"
}

// GetBodyFieldPath returns the binding body's struct field name.
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
	}
	for _, p := range b.PathParams {
		seqs = append(seqs, strings.Split(p.FieldPath.String(), "."))
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

func typeFromName(name string) string {
	lowerName := strings.ToLower(name)
	if strings.Contains(lowerName, "epoch") {
		return "github_com_prysmaticlabs_eth2_types.Epoch"
	} else if strings.Contains(lowerName, "slot") {
		return "github_com_prysmaticlabs_eth2_types.Slot"
	} else if strings.Contains(lowerName, "committee") {
		return "github_com_prysmaticlabs_eth2_types.CommitteeIndex"
	} else if strings.Contains(lowerName, "index") {
		return "github_com_prysmaticlabs_eth2_types.ValidatorIndex"
	}
	return ""
}

func getExtensionValueById(options *descriptorpb.FieldOptions, extensionId int32) (string, error) {
	regex, err := regexp.Compile(fmt.Sprintf("%d:\"([^\"]*)\"", extensionId))
	if err != nil {
		return "", err
	}
	matches := regex.FindStringSubmatch(options.String())
	if len(matches) != 2 {
		return "", nil
	}
	return matches[1], nil
}

func getPkgNameFromTypeString(typeString string) string {
	lastIdx := strings.LastIndex(typeString, ".")
	if lastIdx < 1 {
		return typeString
	}
	secondLastIdx := strings.LastIndex(typeString[:lastIdx], ".")
	return typeString[secondLastIdx+1:]
}

// TODO: Make global message fields list (maybe build this using reflecT?)
func fieldsToTranscodeFunc(msg protoreflect.MessageDescriptor, prefix string, depth uint64) (fields []string, jsonFields []string) {
	var decodeTypeId int32 = 50004

	if depth > 2 || msg == nil {
		return fields, jsonFields
	}

	fieldProtos := msg.Fields()

	log.Printf("Checking message at depth %d: %s\n", depth, msg.Name())
	for i := 0; i < fieldProtos.Len(); i++ {
		ff := fieldProtos.Get(i)
		//log.Printf("Checking field name at depth %d: %s\n", depth, ff.Name())
		//log.Printf("Checking full name at depth %d: %s\n", depth, ff.FullName())
		//log.Printf("Checking json name at depth %d: %s\n", depth, ff.JSONName())
		if ff.Message() != nil {
			log.Printf("Checking field message name at depth %d: %s\n", depth, ff.Message().Name())
			log.Printf("Checking field message reserved name at depth %d: %s\n", depth, ff.Message().ReservedNames())
			log.Printf("Checking field message full name at depth %d: %s\n", depth, ff.Message().FullName())
		}
		options, ok := ff.Options().(*descriptorpb.FieldOptions)
		if !ok {
			log.Println("Skipping options cast")
			continue
		}
		value, err := getExtensionValueById(options, decodeTypeId)
		if err != nil {
			panic(err)
		}
		if value == "hex" {
			fields = append(fields, fmt.Sprintf("%s.%s", prefix, string(ff.Name())))
			jsonFields = append(jsonFields, fmt.Sprintf("%s.%s", prefix, string(ff.FullName())))
		}
		nestedFields, nestedJsonFields := fieldsToTranscodeFunc(ff.Message(), fmt.Sprintf("%s.%s", prefix, ff.JSONName()), depth+1)
		fields = append(fields, nestedFields...)
		jsonFields = append(jsonFields, nestedJsonFields...)
	}


	//log.Printf("Checking message at depth %d: %s\n", depth, *msg.Name)
	//for _, ff := range msg.Fields {
	//	log.Printf("Checking field at depth %d: %s\n", depth, *ff.Name)
	//	value, err := getExtensionValueById(ff, decodeTypeId)
	//	if err != nil {
	//		panic(err)
	//	}
	//	if value == "hex" {
	//		fields = append(fields, fmt.Sprintf("%s.%s", prefix, *ff.Name))
	//		jsonFields = append(jsonFields, fmt.Sprintf("%s.%s", prefix, *ff.JsonName))
	//	}
	//	log.Println(ff.FieldMessage)
	//	nestedFields, nestedJsonFields := fieldsToTranscodeFunc(ff.FieldMessage, fmt.Sprintf("%s.%s", prefix, *ff.JsonName), depth+1)
	//	fields = append(fields, nestedFields...)
	//	jsonFields = append(jsonFields, nestedJsonFields...)
	//}
	return fields, jsonFields
}
func messageFunc(msg *descriptor.Message, prefix string) []string {
	// TODO: somehow find out array lengths at runtime?
	log.Printf("Checking %s\n", *msg.Name)
	_, jsonFieldPaths := fieldsToTranscodeFunc(msg.ProtoReflect().Descriptor(), *msg.Name, 0)
	return jsonFieldPaths
}
//
//func getFullConversionForMessage(file param) func(messageName string) []string {
//	return func(messageName string) []string {
//		innerFile := file
//		for _, mm := range innerFile.Messages {
//		}
//		return []string{}
//	}
//}

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

	messageToDecodeType := make(map[string]map[string]string)
	serviceFieldToDecodeType := make(map[string]map[string]string)
	encodeOutputToMessageType := make(map[string]string)
	jsonFieldNameToGoName := make(map[string]string)
	fieldsToEncode := make(map[string][]string)
	fieldPaths := make(map[string][]string)
	var decodeTypeId int32 = 50004



	decodeInputField := func(functionName string, fieldName string) bool {
		return serviceFieldToDecodeType[functionName][fieldName] == "hex"
	}
	encodeOutputMessageType := func(functionName string) string {
		return encodeOutputToMessageType[functionName]
	}
	jsonFieldNameGoName := func(fieldName string) string {
		return jsonFieldNameToGoName[fieldName]
	}
	funcFieldsEncode := func(functionName string) []string {
		return fieldsToEncode[functionName]
	}
	fieldPathsFunc := func(messageName string) []string {
		return fieldPaths[messageName]
	}

	for _, m := range p.Messages {
		//log.Printf("Message name: %v\n", *m.Name)

		fieldPaths[*m.Name] = messageFunc(m, *m.Name)

		for _, ff := range m.Fields {
			//log.Printf("\tField name: %v\n", *ff.Name)
			//log.Printf("\tField name: %v\n", *ff.JsonName)
			jsonFieldNameToGoName[*ff.Name] = *ff.JsonName

			value, err := getExtensionValueById(ff.Options, decodeTypeId)
			if err != nil {
				return "", err
			}
			if value != "" {
				//log.Printf("\tField Decode_Type Value: %s\n", value)
				if len(messageToDecodeType[m.GoType(*p.Package)]) == 0 {
					messageToDecodeType[m.GoType(*p.Package)] = make(map[string]string)
				}
				messageToDecodeType[m.GoType(*p.Package)][*ff.Name] = value
			}
		}
	}
	for _, ss := range p.Service {
		for _, mm := range ss.Method {
			requestKey := fmt.Sprintf("%s_%s", *ss.Name, *mm.Name)
			//log.Printf("Service Method Name: %v\n", requestKey)

			serviceInputType := getPkgNameFromTypeString(*mm.InputType)
			serviceOutputType := getPkgNameFromTypeString(*mm.OutputType)
			//log.Printf("\tService InputType: %v\n", serviceInputType)
			//log.Printf("\tService OutputType: %v\n", serviceOutputType)

			outputTypeIndex := strings.LastIndex(*mm.OutputType, ".")
			outputType := (*mm.OutputType)[outputTypeIndex+1:]
			encodeOutputToMessageType[requestKey] = outputType

			if len(serviceFieldToDecodeType[requestKey]) == 0 {
				serviceFieldToDecodeType[requestKey] = make(map[string]string)
			}
			for k, v := range messageToDecodeType[serviceInputType] {
				serviceFieldToDecodeType[requestKey][k] = v
			}
			for k := range messageToDecodeType[serviceOutputType] {
				fieldsToEncode[requestKey] = append(fieldsToEncode[requestKey], k)
			}
		}
	}

	for _, svc := range p.Services {
		var methodWithBindingsSeen bool
		svcName := casing.Camel(*svc.Name)
		svc.Name = &svcName

		for _, meth := range svc.Methods {
			glog.V(2).Infof("Processing %s.%s", svc.GetName(), meth.GetName())
			methName := casing.Camel(*meth.Name)
			meth.Name = &methName
			for _, b := range meth.Bindings {

				methodWithBindingsSeen = true
				if err := handlerTemplate.Execute(w, binding{
					Binding:           b,
					Registry:          reg,
					AllowPatchFeature: p.AllowPatchFeature,
					TypeFromName: typeFromName,
					DecodeFromHex: decodeInputField,
					EncodeOutputField: encodeOutputMessageType,
					JsonNameToGoName: jsonFieldNameGoName,
					FieldsToEncode: funcFieldsEncode,
					EncodingFieldPaths: fieldPathsFunc,
				}); err != nil {
					return "", err
				}

				// Local
				if err := localHandlerTemplate.Execute(w, binding{
					Binding:           b,
					Registry:          reg,
					AllowPatchFeature: p.AllowPatchFeature,
					TypeFromName: typeFromName,
					DecodeFromHex: decodeInputField,
					EncodeOutputField: encodeOutputMessageType,
					JsonNameToGoName: jsonFieldNameGoName,
					FieldsToEncode: funcFieldsEncode,
					EncodingFieldPaths: fieldPathsFunc,
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
// Code generated by Prysms protoc-gen-grpc-gateway-cast. DO NOT EDIT.
// source: {{.GetName}}

{{if not .OmitPackageDoc}}/*
Package {{.GoPkg.Name}} is a reverse proxy.

It translates gRPC into RESTful JSON APIs.
*/{{end}}
package {{.GoPkg.Name}}
import (
	"encoding/base64"
	"encoding/hex"
	github_com_prysmaticlabs_eth2_types "github.com/prysmaticlabs/eth2-types"
	emptypb "github.com/golang/protobuf/ptypes/empty"
	"github.com/golang/protobuf/ptypes/empty"
	{{range $i := .Imports}}{{if $i | printf "%q" | ne "github.com/golang/protobuf/ptypes/empty"}}{{$i | printf "%s\n"}}{{end}}{{end}}
)

// Suppress "imported and not used" errors
var _ codes.Code
var _ io.Reader
var _ status.Status
var _ = runtime.String
var _ = utilities.NewDoubleArray
var _ = metadata.Join
var _ = github_com_prysmaticlabs_eth2_types.Epoch(0)
var _ = emptypb.Empty{}
var _ = empty.Empty{}
var _ = base64.Encoding{}
var _ = hex.ErrLength

//func Base64ToHex([]byte) ([]byte, error) {
//  s := base64.StdEncoding.EncodeToString(*h)
//  hexString := hex.EncodeToString([]byte(s))
//
//  // Add quotation marks and '0x' to represent hex value in JSON ("0x...")
//  b := []byte("\"0x"+hexString+"\"")
//
//  return b, nil
//}
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

	_ = template.Must(handlerTemplate.New("request-func-signature").Parse(strings.Replace(`
{{if .Method.GetServerStreaming}}
func request_{{.Method.Service.GetName}}_{{.Method.GetName}}_{{.Index}}(ctx context.Context, marshaler runtime.Marshaler, client {{.Method.Service.InstanceName}}Client, req *http.Request, pathParams map[string]string) ({{.Method.Service.InstanceName}}_{{.Method.GetName}}Client, runtime.ServerMetadata, error)
{{else}}
func request_{{.Method.Service.GetName}}_{{.Method.GetName}}_{{.Index}}(ctx context.Context, marshaler runtime.Marshaler, client {{.Method.Service.InstanceName}}Client, req *http.Request, pathParams map[string]string) (proto.Message, runtime.ServerMetadata, error)
{{end}}`, "\n", "", -1)))

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

	_ = template.Must(handlerTemplate.New("client-rpc-request-func").Parse(`
{{$AllowPatchFeature := .AllowPatchFeature}}
{{$TypeFromName := .TypeFromName}}
{{$DecodeFromHex := .DecodeFromHex}}
{{$EncodeOutputField := .EncodeOutputField}}
{{$JsonNameToGoName := .JsonNameToGoName}}
{{$FieldsToEncode := .FieldsToEncode}}
{{$EncodingFieldPaths := .EncodingFieldPaths}}
{{$MethodName := (printf "%s_%s" .Method.Service.GetName .Method.GetName)}}
{{if .HasQueryParam}}
var (
	filter_{{.Method.Service.GetName}}_{{.Method.GetName}}_{{.Index}} = {{.QueryParamFilter}}
)
{{end}}
{{template "request-func-signature" .}} {
	var protoReq {{.Method.RequestType.GoType .Method.Service.File.GoPkg.Path}}
	var metadata runtime.ServerMetadata
{{if .Body}}
	newReader, berr := utilities.IOReaderFactory(req.Body)
	if berr != nil {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "%v", berr)
	}
	if err := marshaler.NewDecoder(newReader()).Decode(&{{.Body.AssignableExpr "protoReq"}}); err != nil && err != io.EOF  {
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
	{{$fieldName := $param | printf "%v"}}
	val, ok = pathParams[{{$param | printf "%q"}}]
	if !ok {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "missing parameter %s", {{$param | printf "%q"}})
	}
	// {{call $DecodeFromHex $MethodName $fieldName}}
	// {{call $EncodeOutputField $MethodName}}
	// {{$MethodName}}
	// {{$fieldName}}
	// {{call $FieldsToEncode $MethodName}} 
{{if $param.IsNestedProto3}}
	err = runtime.PopulateFieldFromPath(&protoReq, {{$param | printf "%q"}}, val)
	if err != nil {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "type mismatch, parameter: %s, error: %v", {{$param | printf "%q"}}, err)
	}
	{{if $enum}}
		e{{if $param.IsRepeated}}s{{end}}, err = {{$param.ConvertFuncExpr}}(val{{if $param.IsRepeated}}, {{$binding.Registry.GetRepeatedPathParamSeparator | printf "%c" | printf "%q"}}{{end}}, {{$enum.GoType $param.Method.Service.File.GoPkg.Path}}_value)
		if err != nil {
			return nil, metadata, status.Errorf(codes.InvalidArgument, "could not parse path as enum value, parameter: %s, error: %v", {{$param | printf "%q"}}, err)
		}
	{{end}}
{{else if $enum}}
	e{{if $param.IsRepeated}}s{{end}}, err = {{$param.ConvertFuncExpr}}(val{{if $param.IsRepeated}}, {{$binding.Registry.GetRepeatedPathParamSeparator | printf "%c" | printf "%q"}}{{end}}, {{$enum.GoType $param.Method.Service.File.GoPkg.Path}}_value)
	if err != nil {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "type mismatch, parameter: %s, error: %v", {{$param | printf "%q"}}, err)
	}
{{else if call $DecodeFromHex $MethodName $fieldName}}
	// Strip off quotation marks and '0x' from hex value in JSON ("0x...")
	if len(val) > 2 { 
		s := val[2:]
		
		hexBytes, err := hex.DecodeString(s)
		if err != nil {
			return nil, metadata, err
		}
		b64, err := base64.StdEncoding.DecodeString(string(hexBytes))
		if err != nil {
			return nil, metadata, err
		}
		{{$param.AssignableExpr "protoReq"}} = {{$param | printf "%q" | call $TypeFromName}}(b64)
	}
{{else}}
	{{$param}}, err := {{$param.ConvertFuncExpr}}(val{{if $param.IsRepeated}}, {{$binding.Registry.GetRepeatedPathParamSeparator | printf "%c" | printf "%q"}}{{end}})
	if err != nil {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "type mismatch, parameter: %s, error: %v", {{$param | printf "%q"}}, err)
	}
	{{$param.AssignableExpr "protoReq"}} = {{$param | printf "%q" | call $TypeFromName}}({{$param}})
{{end}}
{{if and $enum $param.IsRepeated}}
	s := make([]{{$enum.GoType $param.Method.Service.File.GoPkg.Path}}, len(es))
	for i, v := range es {
		s[i] = {{$enum.GoType $param.Method.Service.File.GoPkg.Path}}(v)
	}
	{{$param.AssignableExpr "protoReq"}} = s
{{else if $enum}}
	{{$param.AssignableExpr "protoReq"}} = {{$enum.GoType $param.Method.Service.File.GoPkg.Path}}(e)
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
{{else if call $EncodeOutputField $MethodName | ne ""}}
	toInterface := func(obj interface{}) interface{} {
		return obj
	}
	msg, err := client.{{.Method.GetName}}(ctx, &protoReq, grpc.Header(&metadata.HeaderMD), grpc.Trailer(&metadata.TrailerMD))
	if err != nil {
		return nil, metadata, err
	}
	castedMsg, ok := toInterface(msg).(*{{call $EncodeOutputField $MethodName}})
	if !ok {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "%v", "hi")
	}
	// {{call $EncodeOutputField $MethodName | call $EncodingFieldPaths}} 
	{{range $param := call $FieldsToEncode $MethodName}}
		castedMsg.{{call $JsonNameToGoName $param}} = ""
	{{end}}
	return toInterface(castedMsg).(proto.Message), metadata, err
{{else}}
	msg, err := client.{{.Method.GetName}}(ctx, &protoReq, grpc.Header(&metadata.HeaderMD), grpc.Trailer(&metadata.TrailerMD))
	return msg, metadata, err
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
	if err := handleSend(); err != nil {
		if cerr := stream.CloseSend(); cerr != nil {
			grpclog.Infof("Failed to terminate client stream: %v", cerr)
		}
		if err == io.EOF {
			return stream, metadata, nil
		}
		return nil, metadata, err
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

	_ = template.Must(localHandlerTemplate.New("local-request-func-signature").Parse(strings.Replace(`
{{if .Method.GetServerStreaming}}
{{else}}
func local_request_{{.Method.Service.GetName}}_{{.Method.GetName}}_{{.Index}}(ctx context.Context, marshaler runtime.Marshaler, server {{.Method.Service.InstanceName}}Server, req *http.Request, pathParams map[string]string) (proto.Message, runtime.ServerMetadata, error)
{{end}}`, "\n", "", -1)))

	_ = template.Must(localHandlerTemplate.New("local-client-rpc-request-func").Parse(`
{{$AllowPatchFeature := .AllowPatchFeature}}
{{$TypeFromName := .TypeFromName}}
{{template "local-request-func-signature" .}} {
	var protoReq {{.Method.RequestType.GoType .Method.Service.File.GoPkg.Path}}
	var metadata runtime.ServerMetadata
{{if .Body}}
	newReader, berr := utilities.IOReaderFactory(req.Body)
	if berr != nil {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "%v", berr)
	}
	if err := marshaler.NewDecoder(newReader()).Decode(&{{.Body.AssignableExpr "protoReq"}}); err != nil && err != io.EOF  {
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
		e{{if $param.IsRepeated}}s{{end}}, err = {{$param.ConvertFuncExpr}}(val{{if $param.IsRepeated}}, {{$binding.Registry.GetRepeatedPathParamSeparator | printf "%c" | printf "%q"}}{{end}}, {{$enum.GoType $param.Method.Service.File.GoPkg.Path}}_value)
		if err != nil {
			return nil, metadata, status.Errorf(codes.InvalidArgument, "could not parse path as enum value, parameter: %s, error: %v", {{$param | printf "%q"}}, err)
		}
	{{end}}
{{else if $enum}}
	e{{if $param.IsRepeated}}s{{end}}, err = {{$param.ConvertFuncExpr}}(val{{if $param.IsRepeated}}, {{$binding.Registry.GetRepeatedPathParamSeparator | printf "%c" | printf "%q"}}{{end}}, {{$enum.GoType $param.Method.Service.File.GoPkg.Path}}_value)
	if err != nil {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "type mismatch, parameter: %s, error: %v", {{$param | printf "%q"}}, err)
	}
{{else}}
	{{$param}}, err := {{$param.ConvertFuncExpr}}(val{{if $param.IsRepeated}}, {{$binding.Registry.GetRepeatedPathParamSeparator | printf "%c" | printf "%q"}}{{end}})
	if err != nil {
		return nil, metadata, status.Errorf(codes.InvalidArgument, "type mismatch, parameter: %s, error: %v", {{$param | printf "%q"}}, err)
	}
	{{$param.AssignableExpr "protoReq"}} = {{$param | printf "%q" | call $TypeFromName}}({{$param}})
{{end}}

{{if and $enum $param.IsRepeated}}
	s := make([]{{$enum.GoType $param.Method.Service.File.GoPkg.Path}}, len(es))
	for i, v := range es {
		s[i] = {{$enum.GoType $param.Method.Service.File.GoPkg.Path}}(v)
	}
	{{$param.AssignableExpr "protoReq"}} = s
{{else if $enum}}
	{{$param.AssignableExpr "protoReq"}} = {{$enum.GoType $param.Method.Service.File.GoPkg.Path}}(e)
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
		rctx, err := runtime.AnnotateIncomingContext(ctx, mux, req, "/{{$svc.File.GetPackage}}.{{$svc.GetName}}/{{$m.GetName}}")
		if err != nil {
			runtime.HTTPError(ctx, mux, outboundMarshaler, w, req, err)
			return
		}
		resp, md, err := local_request_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}(rctx, inboundMarshaler, server, req, pathParams)
		md.HeaderMD, md.TrailerMD = metadata.Join(md.HeaderMD, stream.Header()), metadata.Join(md.TrailerMD, stream.Trailer())
		ctx = runtime.NewServerMetadataContext(ctx, md)
		if err != nil {
			runtime.HTTPError(ctx, mux, outboundMarshaler, w, req, err)
			return
		}

		{{ if $b.ResponseBody }}
		forward_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}(ctx, mux, outboundMarshaler, w, req, response_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}{resp}, mux.GetForwardResponseOptions()...)
		{{ else }}
		forward_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}(ctx, mux, outboundMarshaler, w, req, resp, mux.GetForwardResponseOptions()...)
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
	conn, err := grpc.Dial(endpoint, opts...)
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
		rctx, err := runtime.AnnotateContext(ctx, mux, req, "/{{$svc.File.GetPackage}}.{{$svc.GetName}}/{{$m.GetName}}")
		if err != nil {
			runtime.HTTPError(ctx, mux, outboundMarshaler, w, req, err)
			return
		}
		resp, md, err := request_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}(rctx, inboundMarshaler, client, req, pathParams)
		ctx = runtime.NewServerMetadataContext(ctx, md)
		if err != nil {
			runtime.HTTPError(ctx, mux, outboundMarshaler, w, req, err)
			return
		}
		{{if $m.GetServerStreaming}}
		{{ if $b.ResponseBody }}
		forward_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}(ctx, mux, outboundMarshaler, w, req, func() (proto.Message, error) {
			res, err := resp.Recv()
			return response_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}{res}, err
		}, mux.GetForwardResponseOptions()...)
		{{ else }}
		forward_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}(ctx, mux, outboundMarshaler, w, req, func() (proto.Message, error) { return resp.Recv() }, mux.GetForwardResponseOptions()...)
		{{end}}
		{{else}}
		{{ if $b.ResponseBody }}
		forward_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}(ctx, mux, outboundMarshaler, w, req, response_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}{resp}, mux.GetForwardResponseOptions()...)
		{{ else }}
		forward_{{$svc.GetName}}_{{$m.GetName}}_{{$b.Index}}(ctx, mux, outboundMarshaler, w, req, resp, mux.GetForwardResponseOptions()...)
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
	return {{$b.ResponseBody.AssignableExpr "response"}}
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
)
