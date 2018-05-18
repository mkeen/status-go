package timesource

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/beevik/ntp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// clockCompareDelta declares time required between multiple calls to time.Now
	clockCompareDelta = 30 * time.Microsecond
)

// we don't user real servers for tests, but logic depends on
// actual number of involved NTP servers.
var mockedServers = []string{"ntp1", "ntp2", "ntp3", "ntp4"}

type testCase struct {
	description     string
	servers         []string
	allowedFailures int
	responses       []queryResponse
	expected        time.Duration
	expectError     bool

	// actual attempts are mutable
	mu             sync.Mutex
	actualAttempts int
}

func (tc *testCase) query(string, ntp.QueryOptions) (*ntp.Response, error) {
	tc.mu.Lock()
	defer func() {
		tc.actualAttempts++
		tc.mu.Unlock()
	}()
	response := &ntp.Response{ClockOffset: tc.responses[tc.actualAttempts].Offset}
	return response, tc.responses[tc.actualAttempts].Error
}

func newTestCases() []*testCase {
	return []*testCase{
		{
			description: "SameResponse",
			servers:     mockedServers,
			responses: []queryResponse{
				{Offset: 10 * time.Second},
				{Offset: 10 * time.Second},
				{Offset: 10 * time.Second},
				{Offset: 10 * time.Second},
			},
			expected: 10 * time.Second,
		},
		{
			description: "Median",
			servers:     mockedServers,
			responses: []queryResponse{
				{Offset: 10 * time.Second},
				{Offset: 20 * time.Second},
				{Offset: 20 * time.Second},
				{Offset: 30 * time.Second},
			},
			expected: 20 * time.Second,
		},
		{
			description: "EvenMedian",
			servers:     mockedServers[:2],
			responses: []queryResponse{
				{Offset: 10 * time.Second},
				{Offset: 20 * time.Second},
			},
			expected: 15 * time.Second,
		},
		{
			description: "Error",
			servers:     mockedServers,
			responses: []queryResponse{
				{Offset: 10 * time.Second},
				{Error: errors.New("test")},
				{Offset: 30 * time.Second},
				{Offset: 30 * time.Second},
			},
			expected:    time.Duration(0),
			expectError: true,
		},
		{
			description: "MultiError",
			servers:     mockedServers,
			responses: []queryResponse{
				{Error: errors.New("test 1")},
				{Error: errors.New("test 2")},
				{Error: errors.New("test 3")},
				{Error: errors.New("test 3")},
			},
			expected:    time.Duration(0),
			expectError: true,
		},
		{
			description:     "TolerableError",
			servers:         mockedServers,
			allowedFailures: 1,
			responses: []queryResponse{
				{Offset: 10 * time.Second},
				{Error: errors.New("test")},
				{Offset: 20 * time.Second},
				{Offset: 30 * time.Second},
			},
			expected: 20 * time.Second,
		},
		{
			description:     "NonTolerableError",
			servers:         mockedServers,
			allowedFailures: 1,
			responses: []queryResponse{
				{Offset: 10 * time.Second},
				{Error: errors.New("test")},
				{Error: errors.New("test")},
				{Error: errors.New("test")},
			},
			expected:    time.Duration(0),
			expectError: true,
		},
		{
			description:     "AllFailed",
			servers:         mockedServers,
			allowedFailures: 4,
			responses: []queryResponse{
				{Error: errors.New("test")},
				{Error: errors.New("test")},
				{Error: errors.New("test")},
				{Error: errors.New("test")},
			},
			expected:    time.Duration(0),
			expectError: true,
		},
		{
			description:     "HalfTolerable",
			servers:         mockedServers,
			allowedFailures: 2,
			responses: []queryResponse{
				{Offset: 10 * time.Second},
				{Offset: 20 * time.Second},
				{Error: errors.New("test")},
				{Error: errors.New("test")},
			},
			expected: 15 * time.Second,
		},
	}
}

func TestComputeOffset(t *testing.T) {
	for _, tc := range newTestCases() {
		t.Run(tc.description, func(t *testing.T) {
			offset, err := computeOffset(tc.query, tc.servers, tc.allowedFailures)
			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tc.expected, offset)
		})
	}
}

func TestNTPTimeSource(t *testing.T) {
	for _, tc := range newTestCases() {
		t.Run(tc.description, func(t *testing.T) {
			source := &NTPTimeSource{
				servers:         tc.servers,
				allowedFailures: tc.allowedFailures,
				timeQuery:       tc.query,
			}
			assert.WithinDuration(t, time.Now(), source.Now(), clockCompareDelta)
			err := source.updateOffset()
			if tc.expectError {
				assert.Equal(t, errUpdateOffset, err)
			} else {
				assert.NoError(t, err)
			}
			assert.WithinDuration(t, time.Now().Add(tc.expected), source.Now(), clockCompareDelta)
		})
	}
}

func TestRunningPeriodically(t *testing.T) {
	tc := newTestCases()[0]
	maxHits := 3
	minHits := 1

	t.Run(tc.description, func(t *testing.T) {
		source := &NTPTimeSource{
			servers:           tc.servers,
			allowedFailures:   tc.allowedFailures,
			timeQuery:         tc.query,
			fastNTPSyncPeriod: time.Duration(maxHits*10) * time.Millisecond,
			slowNTPSyncPeriod: time.Duration(minHits*10) * time.Millisecond,
		}

		hits := 0
		var startingTime time.Time
		var endingTime time.Time

		err := source.runPeriodically(func() error {
			hits++
			if hits == 1 {
				startingTime = time.Now()
				return errUpdateOffset
			} else if hits < 3 {
				return errUpdateOffset
			} else if hits == 6 {
				endingTime = time.Now()
				source.wg.Done()
			}
			return nil
		})
		source.wg.Wait()
		require.NoError(t, err)

		minExpected := (2 * maxHits) + (3 * minHits)
		maxExpected := minExpected + 1
		actual := int(endingTime.Sub(startingTime).Seconds() * 100)

		require.True(t, (actual >= minExpected && actual <= maxExpected))
	})
}
