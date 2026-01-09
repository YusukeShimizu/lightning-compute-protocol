package lcpwire

import (
	"errors"
	"fmt"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	quoteTLVTypePriceMsat      = uint64(30)
	quoteTLVTypeQuoteExpiry    = uint64(31)
	quoteTLVTypeTermsHash      = uint64(32)
	quoteTLVTypePaymentRequest = uint64(33)

	quoteTLVTypeResponseContentType     = uint64(34)
	quoteTLVTypeResponseContentEncoding = uint64(35)
)

func EncodeQuote(q Quote) ([]byte, error) {
	envelopeRecords, err := encodeCallEnvelope(q.Envelope)
	if err != nil {
		return nil, err
	}

	if q.PaymentRequest == "" {
		return nil, errors.New("payment_request is required")
	}
	if validateErr := validateUTF8String(q.PaymentRequest, "payment_request"); validateErr != nil {
		return nil, validateErr
	}

	if (q.ResponseContentType != nil) != (q.ResponseContentEncoding != nil) {
		return nil, errors.New("response_content_type and response_content_encoding must be set together")
	}
	if q.ResponseContentType != nil {
		if *q.ResponseContentType == "" {
			return nil, errors.New("response_content_type must be non-empty when present")
		}
		if validateErr := validateUTF8String(*q.ResponseContentType, "response_content_type"); validateErr != nil {
			return nil, validateErr
		}
	}
	if q.ResponseContentEncoding != nil {
		if *q.ResponseContentEncoding == "" {
			return nil, errors.New("response_content_encoding must be non-empty when present")
		}
		if validateErr := validateUTF8String(*q.ResponseContentEncoding, "response_content_encoding"); validateErr != nil {
			return nil, validateErr
		}
	}

	priceMsat := q.PriceMsat
	quoteExpiry := q.QuoteExpiry
	termsHash := [32]byte(q.TermsHash)
	paymentRequestBytes := []byte(q.PaymentRequest)

	records := append([]tlv.Record(nil), envelopeRecords...)
	records = append(records, makeTU64Record(quoteTLVTypePriceMsat, &priceMsat))
	records = append(records, makeTU64Record(quoteTLVTypeQuoteExpiry, &quoteExpiry))
	records = append(records, tlv.MakePrimitiveRecord(tlv.Type(quoteTLVTypeTermsHash), &termsHash))
	records = append(records, tlv.MakePrimitiveRecord(
		tlv.Type(quoteTLVTypePaymentRequest),
		&paymentRequestBytes,
	))

	if q.ResponseContentType != nil {
		responseContentTypeBytes := []byte(*q.ResponseContentType)
		records = append(records, tlv.MakePrimitiveRecord(
			tlv.Type(quoteTLVTypeResponseContentType),
			&responseContentTypeBytes,
		))
	}
	if q.ResponseContentEncoding != nil {
		responseContentEncodingBytes := []byte(*q.ResponseContentEncoding)
		records = append(records, tlv.MakePrimitiveRecord(
			tlv.Type(quoteTLVTypeResponseContentEncoding),
			&responseContentEncodingBytes,
		))
	}

	return encodeTLVStream(records)
}

func DecodeQuote(payload []byte) (Quote, error) {
	m, err := decodeTLVMap(payload)
	if err != nil {
		return Quote{}, err
	}

	env, err := decodeCallEnvelope(m)
	if err != nil {
		return Quote{}, err
	}

	priceBytes, err := requireTLV(m, quoteTLVTypePriceMsat)
	if err != nil {
		return Quote{}, err
	}
	priceMsat, err := readTU64(priceBytes)
	if err != nil {
		return Quote{}, fmt.Errorf("price_msat: %w", err)
	}

	expiryBytes, err := requireTLV(m, quoteTLVTypeQuoteExpiry)
	if err != nil {
		return Quote{}, err
	}
	quoteExpiry, err := readTU64(expiryBytes)
	if err != nil {
		return Quote{}, fmt.Errorf("quote_expiry: %w", err)
	}

	termsHashBytes, err := requireTLV(m, quoteTLVTypeTermsHash)
	if err != nil {
		return Quote{}, err
	}
	termsHash32, err := readBytes32(termsHashBytes)
	if err != nil {
		return Quote{}, fmt.Errorf("terms_hash: %w", err)
	}
	termsHash := lcp.Hash32(termsHash32)

	paymentRequestBytes, err := requireTLV(m, quoteTLVTypePaymentRequest)
	if err != nil {
		return Quote{}, err
	}
	paymentRequest, err := readUTF8String(paymentRequestBytes, "payment_request")
	if err != nil {
		return Quote{}, err
	}
	if paymentRequest == "" {
		return Quote{}, errors.New("payment_request is required")
	}

	var responseContentTypePtr *string
	if b, ok := m[quoteTLVTypeResponseContentType]; ok {
		s, sErr := readUTF8String(b, "response_content_type")
		if sErr != nil {
			return Quote{}, sErr
		}
		if s == "" {
			return Quote{}, errors.New("response_content_type must be non-empty when present")
		}
		responseContentTypePtr = &s
	}

	var responseContentEncodingPtr *string
	if b, ok := m[quoteTLVTypeResponseContentEncoding]; ok {
		s, sErr := readUTF8String(b, "response_content_encoding")
		if sErr != nil {
			return Quote{}, sErr
		}
		if s == "" {
			return Quote{}, errors.New("response_content_encoding must be non-empty when present")
		}
		responseContentEncodingPtr = &s
	}

	if (responseContentTypePtr != nil) != (responseContentEncodingPtr != nil) {
		return Quote{}, errors.New("response_content_type and response_content_encoding must be set together")
	}

	return Quote{
		Envelope:                env,
		PriceMsat:               priceMsat,
		QuoteExpiry:             quoteExpiry,
		TermsHash:               termsHash,
		PaymentRequest:          paymentRequest,
		ResponseContentType:     responseContentTypePtr,
		ResponseContentEncoding: responseContentEncodingPtr,
	}, nil
}
