package federation

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/samsarahq/go/oops"
	"github.com/samson-crypto/thunder/graphql"
)

// serviceSchemas holds all schemas for all of versions of
// all executors services. It is a map from service name
// and version to schema.
type serviceSchemas map[string]map[string]*IntrospectionQueryResult

// FieldInfo holds federation-specific information for
// graphql.Fields used to plan and execute queries.
type FieldInfo struct {
	// Services is the set of all services that can resolve this
	// field. If a service has multiple versions, all versions
	// must be able to resolve the field.
	Services map[string]bool
}

// SchemaWithFederationInfo holds a graphql.Schema along with
// federtion-specific annotations per field.
type SchemaWithFederationInfo struct {
	Schema *graphql.Schema
	// Fields is a map of fields to services which they belong to
	Fields map[*graphql.Field]*FieldInfo
}

func getRootType(typ *introspectionTypeRef) *introspectionTypeRef {
	if typ.OfType == nil {
		return typ
	}
	return getRootType(typ.OfType)
}

// validateFederationKeys validates that if a service is asking for a federated key, all the services
// that have the objcet registered as a root object expose the field. This ensures that we can make
// the hop from the root server to any of the federated servers safely before any queries are executed.
func validateFederationKeys(serviceNames []string, serviceSchemasByName map[string]*IntrospectionQueryResult, obj *graphql.Object, keyField string) error {
	for _, service := range serviceNames {
		for _, typ := range serviceSchemasByName[service].Schema.Types {
			validFederatedKey := false
			if typ.Name == obj.Name {
				// Check that it is a root object by checking if it has a field func called
				// "_federation" on the object
				isRootObject := false
				for _, introspectedField := range typ.Fields {
					if introspectedField.Name == federationField {
						isRootObject = true
						break
					}
				}
				// If it is a root object, check that it has all the fields being requested
				// as a federated key
				if isRootObject {
					for _, introspectedField := range typ.Fields {
						if introspectedField.Name == keyField {
							validFederatedKey = true
							break
						}
					}
				}
				if isRootObject && !validFederatedKey {
					return oops.Errorf("Invalid federation key %s", keyField)
				}
			}
		}
	}
	return nil
}

// validateFederatedObjects validates that if a object is federated, it is federated on all the schemas
func validateFederatedObjects(serviceNames []string, serviceSchemasByName map[string]*IntrospectionQueryResult, objName string) error {
	// Check if it is federated on one service. It is federated if there is a field
	//	called "_federation" on that object on any of the services it is on
	federatedOnOneService := false
	for _, service := range serviceNames {
		for _, typ := range serviceSchemasByName[service].Schema.Types {
			if typ.Name == objName {
				for _, introspectedField := range typ.Fields {
					if introspectedField.Name == federationField {
						federatedOnOneService = true
						break
					}
				}
			}

		}
	}

	if federatedOnOneService && objName != "Query" && objName != "Mutation" {
		// If the object is federated on one service, make sure every service with
		// that object can parse the keys and fetch the object
		for _, service := range serviceNames {
			for _, typ := range serviceSchemasByName[service].Schema.Types {
				if typ.Name == objName {
					isFederatedObject := false
					for _, introspectedField := range typ.Fields {
						if introspectedField.Name == federationField {
							isFederatedObject = true
							break
						}
					}
					if !isFederatedObject {
						return oops.Errorf("Object %s exists on another server and is not federated", objName)
					}
				}
			}
		}
	}
	return nil
}

// validateFieldsReturningFederatedObject checks that if an object is returned by a field func it can not be a shadow object type
func validateFieldsReturningFederatedObject(serviceNames []string, serviceSchemasByName map[string]*IntrospectionQueryResult, types map[string]graphql.Type, fieldInfos map[*graphql.Field]*FieldInfo) error {
	for service, serviceSchema := range serviceSchemasByName {
		for _, typ := range serviceSchema.Schema.Types {
			if typ.Kind != "OBJECT" {
				continue
			}
			for _, field := range typ.Fields {
				// Check that the field's return type is an object
				fieldReturnType := getRootType(field.Type)
				if fieldReturnType.Kind != "OBJECT" {
					continue
				}
				// Error if it is a shadow object. To check this
				// (1) Look through all the fields on the object to see if there is a federation field (_federation) and that
				// the federation field is not on the current service
				// (2) Look through all the fields on the federation object to see if it has a field for <Service>_<ObjectType>
				returnObj, ok := types[fieldReturnType.Name].(*graphql.Object)
				if !ok {
					return oops.Errorf("Return type %s is not a graphql object", fieldReturnType.Name)
				}
				for name, f := range returnObj.Fields {
					if name == federationField && !fieldInfos[f].Services[service] {
						federatedFieldName := fmt.Sprintf("%s_%s", service, fieldReturnType)
						// If the field name is <fieldType-service> on a federation object,
						// it is an expected function for a shadow object type
						if field.Name == federatedFieldName {
							continue
						}
						fedObj, ok := types["Federation"].(*graphql.Object)
						// If there isn't a federation object, it isn't a shadow object
						if !ok {
							continue
						}
						for fName, _ := range fedObj.Fields {
							if fName == federatedFieldName {
								return oops.Errorf("Field func %s can not return shadow type %s", field.Name, returnObj.Name)
							}
						}
					}
				}
			}
		}
	}

	return nil
}

// ConvertVersionedSchemas takes schemas for all of versions of
// all executors services and generates a single merged schema
// annotated with mapping from field to all services that know
// how to resolve the field
func ConvertVersionedSchemas(schemas serviceSchemas) (*SchemaWithFederationInfo, error) {
	serviceNames := make([]string, 0, len(schemas))
	for service := range schemas {
		serviceNames = append(serviceNames, service)
	}
	sort.Strings(serviceNames)

	serviceSchemasByName := make(map[string]*IntrospectionQueryResult)

	// Finds the intersection of different version of the schemas
	var serviceSchemas []*IntrospectionQueryResult
	for _, service := range serviceNames {
		versions := schemas[service]

		versionNames := make([]string, 0, len(versions))
		for version := range versions {
			versionNames = append(versionNames, version)
		}
		sort.Strings(versionNames)

		var versionSchemas []*IntrospectionQueryResult
		for _, version := range versionNames {
			versionSchemas = append(versionSchemas, versions[version])
		}

		serviceSchema, err := mergeSchemaSlice(versionSchemas, Intersection)
		if err != nil {
			return nil, err
		}

		serviceSchemasByName[service] = serviceSchema

		serviceSchemas = append(serviceSchemas, serviceSchema)
	}

	// Finds the union of all the schemas from different executor services
	merged, err := mergeSchemaSlice(serviceSchemas, Union)
	if err != nil {
		return nil, err
	}

	types, err := parseSchema(merged)
	if err != nil {
		return nil, err
	}
	for name := range types {
		if err := validateFederatedObjects(serviceNames, serviceSchemasByName, name); err != nil {
			return nil, oops.Wrapf(err, "Expected all services with object %s to be federated", name)
		}
	}

	fieldInfos := make(map[*graphql.Field]*FieldInfo)
	for _, service := range serviceNames {
		for _, typ := range serviceSchemasByName[service].Schema.Types {
			// For federated fields parse the arguments to figure out which
			// fields are the federated keys. They annotate that information
			// on the field object.
			if typ.Name == "Federation" {
				for _, field := range typ.Fields {
					// Extract the type name from the formatting <service>_<object>
					// And check that the object type exists
					names := strings.SplitN(field.Name, "_", 2)
					if len(names) != 2 {
						return nil, oops.Errorf("Field %s doesnt have an object name and service name", field.Name)
					}
					objName := names[1]
					obj, ok := types[objName].(*graphql.Object)
					if !ok {
						return nil, oops.Errorf("Expected objectName %s on merged schema", objName)
					}

					for _, arg := range field.Args {

						rootType := getRootType(arg.Type)

						inputType, ok := types[rootType.Name].(*graphql.InputObject)
						if !ok {
							return nil, oops.Errorf("Object %s is not an input object, but it is an argument to the field %s", rootType.Name, field.Name)
						}

						// Check that all the input fields are on the federated object
						for fName := range inputType.InputFields {
							if err := validateFederationKeys(serviceNames, serviceSchemasByName, obj, fName); err != nil {
								return nil, err
							}

							if _, ok := obj.Fields[fName]; !ok {
								return nil, oops.Errorf("input field %s is not a field on the object %s", fName, rootType.Name)
							}
						}

						// If the field is one of the input fields to the shadow object func,
						// add the service name to the list of federated keys
						for fName, f := range obj.Fields {
							if _, ok := inputType.InputFields[fName]; !ok {
								continue
							}
							if f.FederatedKey == nil {
								f.FederatedKey = make(map[string]bool, len(serviceNames))
							}
							f.FederatedKey[service] = true
						}
					}
				}
			}
			if typ.Kind == "OBJECT" {
				obj := types[typ.Name].(*graphql.Object)

				for _, field := range typ.Fields {

					f := obj.Fields[field.Name]

					info, ok := fieldInfos[f]
					if !ok {
						info = &FieldInfo{
							Services: map[string]bool{},
						}
						fieldInfos[f] = info
					}
					info.Services[service] = true
				}
			}
		}
	}

	err = validateFieldsReturningFederatedObject(serviceNames, serviceSchemasByName, types, fieldInfos)
	if err != nil {
		return nil, oops.Wrapf(err, "Field funcs can not shadow objects")
	}

	return &SchemaWithFederationInfo{
		Schema: &graphql.Schema{
			Query:    types["Query"],
			Mutation: types["Mutation"],
		},
		Fields: fieldInfos,
	}, nil
}

// convertSchema annotates the schema with federation information vt
// mapping fields to the corresponding services.
func convertSchema(schemas map[string]*IntrospectionQueryResult) (*SchemaWithFederationInfo, error) {
	versionedSchemas := make(serviceSchemas)
	for service, schema := range schemas {
		versionedSchemas[service] = map[string]*IntrospectionQueryResult{
			"": schema,
		}
	}
	return ConvertVersionedSchemas(versionedSchemas)
}

// lookupTypeRef maps the a introspected type to a graphql type
func lookupType(t *introspectionTypeRef, all map[string]graphql.Type) (*introspectionTypeRef, error) {
	if t == nil {
		return nil, errors.New("malformed typeref")
	}
	switch t.Kind {
	case "SCALAR", "OBJECT", "UNION", "INPUT_OBJECT", "ENUM":
		return t, nil
	case "LIST":
		return lookupType(t.OfType, all)
	case "NON_NULL":
		return lookupType(t.OfType, all)
	default:
		return nil, fmt.Errorf("unknown type kind %s", t.Kind)
	}
}

// lookupTypeRef maps the a introspected type to a graphql type
func lookupTypeRef(t *introspectionTypeRef, all map[string]graphql.Type) (graphql.Type, error) {
	if t == nil {
		return nil, errors.New("malformed typeref")
	}

	switch t.Kind {
	case "SCALAR", "OBJECT", "UNION", "INPUT_OBJECT", "ENUM":
		// TODO: enforce type?
		typ, ok := all[t.Name]
		if !ok {
			return nil, fmt.Errorf("type %s not found among top-level types", t.Name)
		}
		return typ, nil

	case "LIST":
		inner, err := lookupTypeRef(t.OfType, all)
		if err != nil {
			return nil, err
		}
		return &graphql.List{
			Type: inner,
		}, nil

	case "NON_NULL":
		inner, err := lookupTypeRef(t.OfType, all)
		if err != nil {
			return nil, err
		}
		return &graphql.NonNull{
			Type: inner,
		}, nil

	default:
		return nil, fmt.Errorf("unknown type kind %s", t.Kind)
	}
}

// parseInputFields maps a list of input types to a list of graphql types
func parseInputFields(source []introspectionInputField, all map[string]graphql.Type) (map[string]graphql.Type, error) {
	fields := make(map[string]graphql.Type)

	for _, field := range source {
		// Validate the inputType is valid
		rawType, err := lookupType(field.Type, all)
		if err != nil {
			return nil, fmt.Errorf("type %s not found", rawType.Name)
		}
		switch rawType.Kind {
		case "INPUT_OBJECT", "SCALAR", "ENUM":
		default:
			return nil, fmt.Errorf("input field %s has bad typ: %s", field.Name, rawType.Kind)
		}

		inputType, err := lookupTypeRef(field.Type, all)
		if err != nil {
			return nil, fmt.Errorf("field %s has bad typ: %v", field.Name, err)
		}
		fields[field.Name] = inputType
	}

	return fields, nil
}

// parseSchema takes the introspected schema, validates the types,
// and maps every field to the graphql types
func parseSchema(schema *IntrospectionQueryResult) (map[string]graphql.Type, error) {
	all := make(map[string]graphql.Type)

	for _, typ := range schema.Schema.Types {
		if _, ok := all[typ.Name]; ok {
			return nil, fmt.Errorf("duplicate type %s", typ.Name)
		}

		switch typ.Kind {
		case "OBJECT":
			all[typ.Name] = &graphql.Object{
				Name: typ.Name,
			}

		case "INPUT_OBJECT":
			all[typ.Name] = &graphql.InputObject{
				Name: typ.Name,
			}

		case "SCALAR":
			all[typ.Name] = &graphql.Scalar{
				Type: typ.Name,
			}

		case "UNION":
			all[typ.Name] = &graphql.Union{
				Name: typ.Name,
			}

		case "ENUM":
			all[typ.Name] = &graphql.Enum{
				Type: typ.Name,
			}

		default:
			return nil, fmt.Errorf("unknown type kind %s", typ.Kind)
		}
	}

	// Initialize barebone types
	for _, typ := range schema.Schema.Types {
		switch typ.Kind {
		case "OBJECT":
			fields := make(map[string]*graphql.Field)
			for _, field := range typ.Fields {
				fieldTyp, err := lookupTypeRef(field.Type, all)
				if err != nil {
					return nil, fmt.Errorf("typ %s field %s has bad typ: %v",
						typ.Name, field.Name, err)
				}

				parsed, err := parseInputFields(field.Args, all)
				if err != nil {
					return nil, fmt.Errorf("field %s input: %v", field.Name, err)
				}

				fields[field.Name] = &graphql.Field{
					Args: parsed,
					Type: fieldTyp,
				}
			}

			all[typ.Name].(*graphql.Object).Fields = fields

		case "INPUT_OBJECT":
			parsed, err := parseInputFields(typ.InputFields, all)
			if err != nil {
				return nil, fmt.Errorf("typ %s: %v", typ.Name, err)
			}

			all[typ.Name].(*graphql.InputObject).InputFields = parsed

		case "UNION":
			types := make(map[string]*graphql.Object)
			for _, other := range typ.PossibleTypes {
				if other.Kind != "OBJECT" {
					return nil, fmt.Errorf("typ %s has possible typ not OBJECT: %v", typ.Name, other)
				}
				typ, ok := all[other.Name].(*graphql.Object)
				if !ok {
					return nil, fmt.Errorf("typ %s possible typ %s does not refer to obj", typ.Name, other.Name)
				}
				types[typ.Name] = typ
			}

			all[typ.Name].(*graphql.Union).Types = types

		case "ENUM":
			// XXX: introspection relies on the EnumValues map.
			reverseMap := make(map[interface{}]string)
			values := make([]string, 0, len(typ.EnumValues))
			for _, value := range typ.EnumValues {
				values = append(values, value.Name)
				reverseMap[value.Name] = value.Name
			}

			enum := all[typ.Name].(*graphql.Enum)
			enum.Values = values
			enum.ReverseMap = reverseMap

		case "SCALAR":
			// pass

		default:
			return nil, fmt.Errorf("unknown type kind %s", typ.Kind)
		}
	}

	return all, nil
}

// XXX: for types missing __federation, take intersection?

// XXX: for (merged) unions, make sure we only send possible types
// to each service

// TODO: support descriptions in merging
