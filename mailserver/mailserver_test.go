// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package mailserver

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/binary"
	"errors"
	"fmt"
	"io/ioutil"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	whisper "github.com/ethereum/go-ethereum/whisper/whisperv6"
	"github.com/status-im/status-go/geth/params"
	"github.com/stretchr/testify/suite"
)

const powRequirement = 0.00001

var keyID string
var seed = time.Now().Unix()

type ServerTestParams struct {
	topic whisper.TopicType
	birth uint32
	low   uint32
	upp   uint32
	key   *ecdsa.PrivateKey
}

func TestMailserverSuite(t *testing.T) {
	suite.Run(t, new(MailserverSuite))
}

type MailserverSuite struct {
	suite.Suite
	server *WMailServer
	shh    *whisper.Whisper
	config *params.WhisperConfig
}

func (s *MailserverSuite) SetupTest() {
	s.server = &WMailServer{}
	s.shh = whisper.New(&whisper.DefaultConfig)
	s.shh.RegisterServer(s.server)
	s.config = &params.WhisperConfig{
		DataDir:             "/tmp/",
		Password:            "pwd",
		MailServerRateLimit: 5,
	}
}

func (s *MailserverSuite) TestInit() {
	testCases := []struct {
		config        params.WhisperConfig
		expectedError error
		limiterActive bool
		info          string
	}{
		{
			config:        params.WhisperConfig{DataDir: ""},
			expectedError: errDirectoryNotProvided,
			limiterActive: false,
			info:          "Initializing a mail server with a config with empty DataDir",
		},
		{
			config:        params.WhisperConfig{DataDir: "/tmp/", Password: ""},
			expectedError: errPasswordNotProvided,
			limiterActive: false,
			info:          "Initializing a mail server with a config with an empty password",
		},
		{
			config:        params.WhisperConfig{DataDir: "/invalid-path", Password: "pwd"},
			expectedError: errors.New("open DB: mkdir /invalid-path: permission denied"),
			limiterActive: false,
			info:          "Initializing a mail server with a config with an unexisting DataDir",
		},
		{
			config:        *s.config,
			expectedError: nil,
			limiterActive: true,
			info:          "Initializing a mail server with a config with correct config and active limiter",
		},
		{
			config: params.WhisperConfig{
				DataDir:  "/tmp/",
				Password: "pwd",
			},
			expectedError: nil,
			limiterActive: false,
			info:          "Initializing a mail server with a config with empty DataDir and inactive limiter",
		},
	}

	for _, tc := range testCases {
		s.T().Run(tc.info, func(*testing.T) {
			s.server.limit = nil
			err := s.server.Init(s.shh, &tc.config)
			s.server.tick = nil
			s.server.Close()
			s.Equal(tc.expectedError, err)
			s.Equal(tc.limiterActive, (s.server.limit != nil))
		})
	}
}

func (s *MailserverSuite) TestArchive() {
	err := s.server.Init(s.shh, s.config)
	s.server.tick = nil
	s.NoError(err)
	defer s.server.Close()

	env, err := generateEnvelope(time.Now())
	s.NoError(err)
	rawEnvelope, err := rlp.EncodeToBytes(env)
	s.NoError(err)

	s.server.Archive(env)
	key := NewDbKey(env.Expiry-env.TTL, env.Hash())
	archivedEnvelope, err := s.server.db.Get(key.raw, nil)
	s.NoError(err)

	s.Equal(rawEnvelope, archivedEnvelope)
}

func (s *MailserverSuite) TestManageLimits() {
	s.server.limit = newLimiter(time.Duration(5) * time.Millisecond)
	s.server.managePeerLimits([]byte("peerID"))
	s.Equal(1, len(s.server.limit.db))
	firstSaved := s.server.limit.db["peerID"]

	// second call when limit is not accomplished does not store a new limit
	s.server.managePeerLimits([]byte("peerID"))
	s.Equal(1, len(s.server.limit.db))
	s.Equal(firstSaved, s.server.limit.db["peerID"])
}

func (s *MailserverSuite) TestDBKey() {
	var h common.Hash
	i := uint32(time.Now().Unix())
	k := NewDbKey(i, h)
	s.Equal(len(k.raw), common.HashLength+4, "wrong DB key length")
	s.Equal(byte(i%0x100), k.raw[3], "raw representation should be big endian")
	s.Equal(byte(i/0x1000000), k.raw[0], "big endian expected")
}

func (s *MailserverSuite) TestMailServer() {
	var server WMailServer

	s.setupServer(&server)
	defer server.Close()

	env, err := generateEnvelope(time.Now())
	s.NoError(err)

	server.Archive(env)
	testCases := []struct {
		params      *ServerTestParams
		emptyLow    bool
		lowModifier int32
		uppModifier int32
		topic       byte
		expect      bool
		shouldFail  bool
		info        string
	}{
		{
			params:      s.defaultServerParams(env),
			lowModifier: 0,
			uppModifier: 0,
			expect:      true,
			shouldFail:  false,
			info:        "Processing a request where from and to are equals to an existing register, should provide results",
		},
		{
			params:      s.defaultServerParams(env),
			lowModifier: 1,
			uppModifier: 1,
			expect:      false,
			shouldFail:  false,
			info:        "Processing a request where from and to are great than any existing register, should not provide results",
		},
		{
			params:      s.defaultServerParams(env),
			lowModifier: 0,
			uppModifier: 1,
			topic:       0xFF,
			expect:      false,
			shouldFail:  false,
			info:        "Processing a request where to is grat than any existing register and with a specific topic, should not provide results",
		},
		{
			params:      s.defaultServerParams(env),
			emptyLow:    true,
			lowModifier: 4,
			uppModifier: -1,
			shouldFail:  true,
			info:        "Processing a request where to is lower than from should fail",
		},
		{
			params:      s.defaultServerParams(env),
			emptyLow:    true,
			lowModifier: 0,
			uppModifier: 24,
			shouldFail:  true,
			info:        "Processing a request where difference between from and to is > 24 should fail",
		},
	}
	for _, tc := range testCases {
		s.T().Run(tc.info, func(*testing.T) {
			if tc.lowModifier != 0 {
				tc.params.low = tc.params.birth + uint32(tc.lowModifier)
			}
			if tc.uppModifier != 0 {
				tc.params.upp = tc.params.birth + uint32(tc.uppModifier)
			}
			if tc.emptyLow {
				tc.params.low = 0
			}
			if tc.topic == 0xFF {
				tc.params.topic[0] = tc.topic
			}

			request := s.createRequest(tc.params)
			src := crypto.FromECDSAPub(&tc.params.key.PublicKey)
			ok, lower, upper, bloom := server.validateRequest(src, request)
			if tc.shouldFail {
				if ok {
					s.T().Fatal(err)
				}
				return
			}
			if !ok {
				s.T().Fatalf("request validation failed, seed: %d.", seed)
			}
			if lower != tc.params.low {
				s.T().Fatalf("request validation failed (lower bound), seed: %d.", seed)
			}
			if upper != tc.params.upp {
				s.T().Fatalf("request validation failed (upper bound), seed: %d.", seed)
			}
			expectedBloom := whisper.TopicToBloom(tc.params.topic)
			if !bytes.Equal(bloom, expectedBloom) {
				s.T().Fatalf("request validation failed (topic), seed: %d.", seed)
			}

			var exist bool
			mail := server.processRequest(nil, tc.params.low, tc.params.upp, bloom)
			for _, msg := range mail {
				if msg.Hash() == env.Hash() {
					exist = true
					break
				}
			}

			if exist != tc.expect {
				s.T().Fatalf("error: exist = %v, seed: %d.", exist, seed)
			}

			src[0]++
			ok, lower, upper, _ = server.validateRequest(src, request)
			if !ok {
				// request should be valid regardless of signature
				s.T().Fatalf("request validation false negative, seed: %d (lower: %d, upper: %d).", seed, lower, upper)
			}
		})
	}
}

func (s *MailserverSuite) TestBloomFromReceivedMessage() {
	testCases := []struct {
		msg           whisper.ReceivedMessage
		expectedBloom []byte
		expectedErr   error
		info          string
	}{
		{
			msg:           whisper.ReceivedMessage{},
			expectedBloom: []byte(nil),
			expectedErr:   errors.New("Undersized p2p request"),
			info:          "getting bloom filter for an empty whisper message should produce an error",
		},
		{
			msg:           whisper.ReceivedMessage{Payload: []byte("hohohohoho")},
			expectedBloom: []byte(nil),
			expectedErr:   errors.New("Undersized bloom filter in p2p request"),
			info:          "getting bloom filter for a malformed whisper message should produce an error",
		},
		{
			msg:           whisper.ReceivedMessage{Payload: []byte("12345678")},
			expectedBloom: whisper.MakeFullNodeBloom(),
			expectedErr:   nil,
			info:          "getting bloom filter for a valid whisper message should be successful",
		},
	}

	for _, tc := range testCases {
		s.T().Run(tc.info, func(*testing.T) {
			bloom, err := s.server.bloomFromReceivedMessage(&tc.msg)
			s.Equal(tc.expectedErr, err)
			s.Equal(tc.expectedBloom, bloom)
		})
	}
}

func (s *MailserverSuite) setupServer(server *WMailServer) {
	const password = "password_for_this_test"
	const dbPath = "whisper-server-test"

	dir, err := ioutil.TempDir("", dbPath)
	if err != nil {
		s.T().Fatal(err)
	}

	s.shh = whisper.New(&whisper.DefaultConfig)
	s.shh.RegisterServer(server)

	err = server.Init(s.shh, &params.WhisperConfig{DataDir: dir, Password: password, MinimumPoW: powRequirement})
	if err != nil {
		s.T().Fatal(err)
	}

	keyID, err = s.shh.AddSymKeyFromPassword(password)
	if err != nil {
		s.T().Fatalf("failed to create symmetric key for mail request: %s", err)
	}
}

func (s *MailserverSuite) defaultServerParams(env *whisper.Envelope) *ServerTestParams {
	id, err := s.shh.NewKeyPair()
	if err != nil {
		s.T().Fatalf("failed to generate new key pair with seed %d: %s.", seed, err)
	}
	testPeerID, err := s.shh.GetPrivateKey(id)
	if err != nil {
		s.T().Fatalf("failed to retrieve new key pair with seed %d: %s.", seed, err)
	}
	birth := env.Expiry - env.TTL

	return &ServerTestParams{
		topic: env.Topic,
		birth: birth,
		low:   birth - 1,
		upp:   birth + 1,
		key:   testPeerID,
	}
}

func (s *MailserverSuite) createRequest(p *ServerTestParams) *whisper.Envelope {
	bloom := whisper.TopicToBloom(p.topic)
	data := make([]byte, 8)
	binary.BigEndian.PutUint32(data, p.low)
	binary.BigEndian.PutUint32(data[4:], p.upp)
	data = append(data, bloom...)

	key, err := s.shh.GetSymKey(keyID)
	if err != nil {
		s.T().Fatalf("failed to retrieve sym key with seed %d: %s.", seed, err)
	}

	params := &whisper.MessageParams{
		KeySym:   key,
		Topic:    p.topic,
		Payload:  data,
		PoW:      powRequirement * 2,
		WorkTime: 2,
		Src:      p.key,
	}

	msg, err := whisper.NewSentMessage(params)
	if err != nil {
		s.T().Fatalf("failed to create new message with seed %d: %s.", seed, err)
	}
	env, err := msg.Wrap(params, time.Now())
	if err != nil {
		s.T().Fatalf("failed to wrap with seed %d: %s.", seed, err)
	}
	return env
}

func generateEnvelope(sentTime time.Time) (*whisper.Envelope, error) {
	h := crypto.Keccak256Hash([]byte("test sample data"))
	params := &whisper.MessageParams{
		KeySym:   h[:],
		Topic:    whisper.TopicType{0x1F, 0x7E, 0xA1, 0x7F},
		Payload:  []byte("test payload"),
		PoW:      powRequirement,
		WorkTime: 2,
	}

	msg, err := whisper.NewSentMessage(params)
	if err != nil {
		return nil, fmt.Errorf("failed to create new message with seed %d: %s", seed, err)
	}
	env, err := msg.Wrap(params, sentTime)
	if err != nil {
		return nil, fmt.Errorf("failed to wrap with seed %d: %s", seed, err)
	}

	return env, nil
}
