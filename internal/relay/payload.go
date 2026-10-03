package relay

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func carriesUnreadableFields(message proto.Message) bool {
	if message == nil {
		return false
	}
	return hasUnknown(message.ProtoReflect())
}

func hasUnknown(message protoreflect.Message) bool {
	if !message.IsValid() {
		return false
	}
	if len(message.GetUnknown()) > 0 {
		return true
	}

	unreadable := false
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		unreadable = fieldCarriesUnknown(field, value)
		return !unreadable
	})
	return unreadable
}

func fieldCarriesUnknown(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
	switch {
	case field.IsMap():
		unreadable := false
		value.Map().Range(func(_ protoreflect.MapKey, entry protoreflect.Value) bool {
			if field.MapValue().Message() != nil {
				unreadable = hasUnknown(entry.Message())
			}
			return !unreadable
		})
		return unreadable

	case field.IsList() && field.Message() != nil:
		list := value.List()
		for index := range list.Len() {
			if hasUnknown(list.Get(index).Message()) {
				return true
			}
		}
		return false

	case field.Kind() == protoreflect.MessageKind || field.Kind() == protoreflect.GroupKind:
		return hasUnknown(value.Message())

	default:
		return false
	}
}
