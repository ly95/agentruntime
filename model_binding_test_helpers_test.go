package agentruntime

var defaultTestModelBinding = ModelBinding{
	Provider:            "test-provider",
	Model:               "test-model",
	EndpointClass:       "process-local-test",
	CredentialPrincipal: "test-principal",
	AdapterVersion:      "test-adapter-v1",
}

func defaultTestModelBindingID() string {
	id, err := defaultTestModelBinding.ID()
	if err != nil {
		panic(err)
	}
	return id
}

type boundTestModel struct {
	Model
	binding ModelBinding
}

func (model boundTestModel) Binding() ModelBinding { return model.binding }

func withDefaultTestModelBinding(model Model) Model {
	if _, ok := model.(BoundModel); ok {
		return model
	}
	return boundTestModel{Model: model, binding: defaultTestModelBinding}
}

func (*scriptedModel) Binding() ModelBinding            { return defaultTestModelBinding }
func (streamCallbackModel) Binding() ModelBinding       { return defaultTestModelBinding }
func (failingModel) Binding() ModelBinding              { return defaultTestModelBinding }
func (*streamSinkObservingModel) Binding() ModelBinding { return defaultTestModelBinding }
func (*multiTurnChunkModel) Binding() ModelBinding      { return defaultTestModelBinding }
func (*mutatingRequestModel) Binding() ModelBinding     { return defaultTestModelBinding }
func (blockingModel) Binding() ModelBinding             { return defaultTestModelBinding }
func (cancelingModel) Binding() ModelBinding            { return defaultTestModelBinding }
func (invalidUTF8StreamModel) Binding() ModelBinding    { return defaultTestModelBinding }
func (errorModel) Binding() ModelBinding                { return defaultTestModelBinding }
func (failureAuditModel) Binding() ModelBinding         { return defaultTestModelBinding }
