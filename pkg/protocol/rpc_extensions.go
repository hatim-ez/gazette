package protocol

import "net/url"

// Validate returns an error if the ReadRequest is not well-formed.
func (m *ReadRequest) Validate() error {
	if err := m.Journal.Validate(); err != nil {
		return ExtendContext(err, "Journal")
	} else if m.Offset < -1 {
		return NewValidationError("invalid Offset (%d; expected -1 <= Offset <= MaxInt64)", m.Offset)
	}

	// Block, DoNotProxy, and MetadataOnly (each type bool) require no extra validation.

	return nil
}

// Validate returns an error if the ReadResponse is not well-formed.
func (m *ReadResponse) Validate() error {
	if err := m.Status.Validate(); err != nil {
		return ExtendContext(err, "Status")
	}

	if m.Content != nil {
		// Require that no other fields are set.
		if m.Status != Status_OK {
			return NewValidationError("unexpected Status with Content (%v)", m.Status)
		} else if m.Offset != 0 {
			return NewValidationError("unexpected Offset with Content (%d)", m.Offset)
		} else if m.WriteHead != 0 {
			return NewValidationError("unexpected WriteHead with Content (%d)", m.WriteHead)
		} else if m.Route != nil {
			return NewValidationError("unexpected Route with Content (%s)", m.Route)
		} else if m.Fragment != nil {
			return NewValidationError("unexpected Fragment with Content (%s)", m.Fragment)
		} else if m.FragmentUrl != "" {
			return NewValidationError("unexpected FragmentUrl with Content (%s)", m.FragmentUrl)
		}
		return nil
	}

	if m.Route != nil {
		if err := m.Route.Validate(); err != nil {
			return ExtendContext(err, "Route")
		}
	}
	if m.WriteHead < 0 {
		return NewValidationError("invalid WriteHead (%d; expected >= 0)", m.WriteHead)
	}

	if m.Fragment != nil {
		if err := m.Fragment.Validate(); err != nil {
			return ExtendContext(err, "Fragment")
		} else if m.Offset < m.Fragment.Begin || m.Offset >= m.Fragment.End {
			return NewValidationError("invalid Offset (%d; expected %d <= offset < %d)",
				m.Offset, m.Fragment.Begin, m.Fragment.End)
		} else if m.WriteHead < m.Fragment.End {
			return NewValidationError("invalid WriteHead (%d; expected >= %d)",
				m.WriteHead, m.Fragment.End)
		}
		if m.FragmentUrl != "" {
			if _, err := url.Parse(m.FragmentUrl); err != nil {
				return ExtendContext(&ValidationError{Err: err}, "FragmentUrl")
			}
		}
	} else {
		if m.Offset != 0 {
			return NewValidationError("unexpected Offset without Fragment (%d)", m.Offset)
		} else if m.FragmentUrl != "" {
			return NewValidationError("unexpected FragmentUrl without Fragment (%s)", m.FragmentUrl)
		}
	}

	return nil
}

// Validate returns an error if the AppendRequest is not well-formed.
func (m *AppendRequest) Validate() error {
	if m.Journal != "" {
		if err := m.Journal.Validate(); err != nil {
			return ExtendContext(err, "Journal")
		} else if len(m.Content) != 0 {
			return NewValidationError("unexpected Content")
		}
	} else if len(m.Content) == 0 {
		return NewValidationError("expected Content")
	}
	return nil
}

// Validate returns an error if the AppendResponse is not well-formed.
func (m *AppendResponse) Validate() error {
	if err := m.Status.Validate(); err != nil {
		return ExtendContext(err, "Status")
	} else if m.Route == nil {
		return NewValidationError("expected Route")
	} else if err = m.Route.Validate(); err != nil {
		return ExtendContext(err, "Route")
	} else if m.Commit == nil {
		return NewValidationError("expected Commit")
	} else if err = m.Commit.Validate(); err != nil {
		return ExtendContext(err, "Commit")
	}
	return nil
}

// Validate returns an error if the ReplicateRequest is not well-formed.
func (m *ReplicateRequest) Validate() error {

	if m.Journal != "" {
		// This is an initial request.

		if err := m.Journal.Validate(); err != nil {
			return ExtendContext(err, "Journal")
		} else if m.Route == nil {
			return NewValidationError("expected Route with Journal")
		} else if err := m.Route.Validate(); err != nil {
			return ExtendContext(err, "Route")
		} else if m.Proposal == nil {
			return NewValidationError("expected Proposal with Journal")
		} else if err := m.Proposal.Validate(); err != nil {
			return ExtendContext(err, "Proposal")
		} else if m.Proposal.Journal != m.Journal {
			return NewValidationError("Journal and Proposal.Journal mismatch (%s vs %s)", m.Journal, m.Proposal.Journal)
		} else if m.Content != nil {
			return NewValidationError("unexpected Content with Journal (len %d)", len(m.Content))
		} else if m.ContentDelta != 0 {
			return NewValidationError("unexpected ContentDelta with Journal (%d)", m.ContentDelta)
		} else if m.Acknowledge == false {
			return NewValidationError("expected Acknowledge with Journal")
		}
		return nil
	}

	if m.Route != nil {
		return NewValidationError("unexpected Route without Journal (%s)", m.Route)
	}

	if m.Proposal != nil {
		if err := m.Proposal.Validate(); err != nil {
			return ExtendContext(err, "Proposal")
		} else if m.Content != nil {
			return NewValidationError("unexpected Content with Proposal (len %d)", len(m.Content))
		} else if m.ContentDelta != 0 {
			return NewValidationError("unexpected ContentDelta with Proposal (%d)", m.ContentDelta)
		}
		return nil
	}

	if m.Content == nil {
		return NewValidationError("expected Content or Proposal")
	}
	if m.Acknowledge {
		return NewValidationError("unexpected Acknowledge with Content")
	}
	if m.ContentDelta < 0 {
		return NewValidationError("invalid ContentDelta (%d; expected >= 0)", m.ContentDelta)
	}

	return nil
}

// Validate returns an error if the ReplicateResponse is not well-formed.
func (m *ReplicateResponse) Validate() error {
	if err := m.Status.Validate(); err != nil {
		return ExtendContext(err, "Status")
	}

	if m.Status == Status_WRONG_ROUTE {
		if m.Route == nil {
			return NewValidationError("expected Route")
		} else if err := m.Route.Validate(); err != nil {
			return ExtendContext(err, "Route")
		}
	} else if m.Route != nil {
		return NewValidationError("unexpected Route (%s)", m.Route)
	}

	if m.Status == Status_FRAGMENT_MISMATCH {
		if m.Fragment == nil {
			return NewValidationError("expected Fragment")
		} else if err := m.Fragment.Validate(); err != nil {
			return ExtendContext(err, "Fragment")
		}
	} else if m.Fragment != nil {
		return NewValidationError("unexpected Fragment (%s)", m.Fragment)
	}

	return nil
}

// Validate returns an error if the Status is not well-formed.
func (x Status) Validate() error {
	if _, ok := Status_name[int32(x)]; !ok {
		return NewValidationError("invalid status (%s)", x)
	}
	return nil
}