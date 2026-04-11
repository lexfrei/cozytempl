package k8s

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
)

// corev1GV is the group-version for core Kubernetes resources. Used
// by LogService to build a REST client that targets /api/v1 rather
// than /apis/.
//
//nolint:gochecknoglobals // typed constant, read-only
var corev1GV = schema.GroupVersion{Group: "", Version: "v1"}

// basicSerializer is a minimal NegotiatedSerializer implementation
// that returns a no-frills JSON codec. The /log subresource returns
// text, so we never actually need to decode — but rest.RESTClientFor
// requires a non-nil serializer regardless.
type basicSerializer struct{}

// SupportedMediaTypes lists the media types our codec handles. A
// single JSON entry is enough to satisfy the REST client.
func (basicSerializer) SupportedMediaTypes() []runtime.SerializerInfo {
	return []runtime.SerializerInfo{{
		MediaType:        "application/json",
		MediaTypeType:    "application",
		MediaTypeSubType: "json",
		EncodesAsText:    true,
		Serializer:       json.NewSerializer(json.DefaultMetaFactory, nil, nil, false),
	}}
}

// EncoderForVersion is unused by our single REST call (we only Get,
// never Post), but the interface demands it.
func (basicSerializer) EncoderForVersion(encoder runtime.Encoder, _ runtime.GroupVersioner) runtime.Encoder {
	return encoder
}

// DecoderToVersion returns the decoder unmodified; the /log endpoint
// returns a byte stream so no conversion is needed.
func (basicSerializer) DecoderToVersion(decoder runtime.Decoder, _ runtime.GroupVersioner) runtime.Decoder {
	return decoder
}

var _ runtime.NegotiatedSerializer = basicSerializer{}

// unused serializer keeps the json import alive even if the file is
// trimmed later.
var _ = serializer.CodecFactory{}
