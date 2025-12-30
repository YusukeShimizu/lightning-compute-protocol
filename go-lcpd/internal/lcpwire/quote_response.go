package lcpwire

import (
	"errors"
	"fmt"

	"github.com/bruwbird/lcp/go-lcpd/internal/domain/lcp"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	quoteResponseTLVTypePriceMsat      = uint64(30)
	quoteResponseTLVTypeQuoteExpiry    = uint64(31)
	quoteResponseTLVTypeTermsHash      = uint64(32)
	quoteResponseTLVTypePaymentRequest = uint64(33)
)

func EncodeQuoteResponse(q QuoteResponse) ([]byte, error) {
	envelopeRecords, err := encodeJobEnvelope(q.Envelope)
	if err != nil {
		return nil, err
	}

	if q.PaymentRequest == "" {
		return nil, errors.New("payment_request is required")
	}
	if validateErr := validateUTF8String(q.PaymentRequest, "payment_request"); validateErr != nil {
		return nil, validateErr
	}

	priceMsat := q.PriceMsat
	quoteExpiry := q.QuoteExpiry
	termsHash := [32]byte(q.TermsHash)
	paymentRequestBytes := []byte(q.PaymentRequest)

	records := append([]tlv.Record(nil), envelopeRecords...)
	records = append(records, makeTU64Record(quoteResponseTLVTypePriceMsat, &priceMsat))
	records = append(records, makeTU64Record(quoteResponseTLVTypeQuoteExpiry, &quoteExpiry))
	records = append(
		records,
		tlv.MakePrimitiveRecord(tlv.Type(quoteResponseTLVTypeTermsHash), &termsHash),
	)
	records = append(records, tlv.MakePrimitiveRecord(
		tlv.Type(quoteResponseTLVTypePaymentRequest),
		&paymentRequestBytes,
	))

	return encodeTLVStream(records)
}

func DecodeQuoteResponse(payload []byte) (QuoteResponse, error) {
	m, err := decodeTLVMap(payload)
	if err != nil {
		return QuoteResponse{}, err
	}

	env, err := decodeJobEnvelope(m)
	if err != nil {
		return QuoteResponse{}, err
	}

	priceBytes, err := requireTLV(m, quoteResponseTLVTypePriceMsat)
	if err != nil {
		return QuoteResponse{}, err
	}
	priceMsat, err := readTU64(priceBytes)
	if err != nil {
		return QuoteResponse{}, fmt.Errorf("price_msat: %w", err)
	}

	expiryBytes, err := requireTLV(m, quoteResponseTLVTypeQuoteExpiry)
	if err != nil {
		return QuoteResponse{}, err
	}
	quoteExpiry, err := readTU64(expiryBytes)
	if err != nil {
		return QuoteResponse{}, fmt.Errorf("quote_expiry: %w", err)
	}

	termsHashBytes, err := requireTLV(m, quoteResponseTLVTypeTermsHash)
	if err != nil {
		return QuoteResponse{}, err
	}
	termsHash32, err := readBytes32(termsHashBytes)
	if err != nil {
		return QuoteResponse{}, fmt.Errorf("terms_hash: %w", err)
	}
	termsHash := lcp.Hash32(termsHash32)

	paymentRequestBytes, err := requireTLV(m, quoteResponseTLVTypePaymentRequest)
	if err != nil {
		return QuoteResponse{}, err
	}
	paymentRequest, err := readUTF8String(paymentRequestBytes, "payment_request")
	if err != nil {
		return QuoteResponse{}, err
	}
	if paymentRequest == "" {
		return QuoteResponse{}, errors.New("payment_request is required")
	}

	return QuoteResponse{
		Envelope:       env,
		PriceMsat:      priceMsat,
		QuoteExpiry:    quoteExpiry,
		TermsHash:      termsHash,
		PaymentRequest: paymentRequest,
	}, nil
}
