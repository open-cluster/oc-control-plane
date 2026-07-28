package relay

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// A capability payload carrying fields this control plane cannot see is refused rather than
// accepted with the unreadable parts ignored.
//
// The reason is what the platform is for. Evidence is only worth what its provenance is worth,
// and a result whose schema this side does not fully understand cannot be described honestly:
// recording it would state that a workload was read and understood when part of what came back
// was never looked at. A missing capability version, a relay built against a newer schema, a
// payload assembled by something else entirely — all arrive looking exactly like this, and all
// of them mean the same thing, which is that the two sides do not agree on what was said.
//
// Only capability payloads are checked. The envelope around them evolves additively by design,
// so a newer relay saying more than this control plane understands at that level is expected
// and must not be refused.

// carriesUnreadableFields reports whether a message, or anything nested inside it, carries
// fields this build does not know about. The walk is recursive because a schema change is as
// likely to be three levels down in a container's state as it is at the top.
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
		// Ranging stops at the first one found: the answer cannot change, and a payload built
		// to be expensive to inspect must not be able to make this walk the expensive part.
		return !unreadable
	})
	return unreadable
}

func fieldCarriesUnknown(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
	switch {
	case field.IsMap():
		// The contract forbids maps in these payloads, so this is unreachable through a
		// conforming schema. It is walked anyway, because a walk that trusts the schema it is
		// checking is not a check.
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
