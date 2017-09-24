package pacemaker

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/net/context"

	"github.com/beevik/etree"
	"github.com/Sirupsen/logrus"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type fakeCib struct{ mock.Mock }

func (c fakeCib) Get(xpaths ...string) ([]*etree.Element, error) {
	args := c.Called(xpaths)
	return args.Get(0).([]*etree.Element), args.Error(1)
}

type fakeHandler struct{ mock.Mock }

func (h fakeHandler) Run(key, value string) error {
	args := h.Called(key, value)
	return args.Error(0)
}

func fakeTicker(ctx context.Context) (*time.Ticker, func()) {
	tickChan := make(chan time.Time)
	ticker := time.Ticker{C: tickChan}

	return &ticker, func() {
		// Non-blocking send, so we can repeatedly tick until the end of time
		select {
		case tickChan <- time.Now():
		default:
		}
	}
}

func TestStart(t *testing.T) {
	makeResources := func(values ...string) []*etree.Element {
		elements := make([]*etree.Element, len(values))

		for idx, value := range values {
			elements[idx] = etree.NewElement("resource")
			elements[idx].CreateAttr("name", value)
		}

		return elements
	}

	testCases := []struct {
		name       string
		nodes      []*crmNode
		getParams  []string
		getResults [][]*etree.Element
		handlers   map[string]handler
	}{
		{
			"when node changes, handler is called",
			[]*crmNode{
				&crmNode{
					Alias:     "/master",
					XPath:     "/resource[@id='PostgresqlVIP']",
					Attribute: "name",
				},
			},
			[]string{"/resource[@id='PostgresqlVIP']"},
			[][]*etree.Element{
				makeResources("larry"),
				makeResources("moe"),
			},
			func() map[string]handler {
				h := new(fakeHandler)

				h.On("Run", "/master", "larry").Return(nil).Once()
				h.On("Run", "/master", "moe").Return(nil).Once()

				return map[string]handler{"/master": h}
			}(),
		},
		{
			"when nodes don't change between polling, handler is only called once",
			[]*crmNode{
				&crmNode{
					Alias:     "/master",
					XPath:     "/resource[@id='PostgresqlVIP']",
					Attribute: "name",
				},
			},
			[]string{"/resource[@id='PostgresqlVIP']"},
			[][]*etree.Element{
				makeResources("larry"),
				makeResources("larry"),
				makeResources("larry"),
			},
			func() map[string]handler {
				h := new(fakeHandler)
				h.On("Run", "/master", "larry").Return(nil).Once()

				return map[string]handler{"/master": h}
			}(),
		},
		{
			"when watching multiple nodes, we call the right handlers",
			[]*crmNode{
				&crmNode{
					Alias:     "/master",
					XPath:     "/resource[@id='PostgresqlVIP']",
					Attribute: "name",
				},
				&crmNode{
					Alias:     "/pgbouncer",
					XPath:     "/resource[@id='PgBouncerVIP']",
					Attribute: "name",
				},
			},
			[]string{"/resource[@id='PostgresqlVIP']", "/resource[@id='PgBouncerVIP']"},
			[][]*etree.Element{
				makeResources("larry", "curly"),
				makeResources("larry", "moe"),
				makeResources("curly", "moe"),
			},
			func() map[string]handler {
				masterHandler := new(fakeHandler)
				masterHandler.On("Run", "/master", "larry").Return(nil).Once()
				masterHandler.On("Run", "/master", "curly").Return(nil).Once()

				bouncerHandler := new(fakeHandler)
				bouncerHandler.On("Run", "/pgbouncer", "curly").Return(nil).Once()
				bouncerHandler.On("Run", "/pgbouncer", "moe").Return(nil).Once()

				return map[string]handler{"/master": masterHandler, "/pgbouncer": bouncerHandler}
			}(),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Stub cib to expect the given tc.getParams, returning tc.getResults.
			cib := new(fakeCib)
			for _, results := range tc.getResults {
				cib.On("Get", tc.getParams).Return(results, nil).Once()
			}

			// This last stub will cancel our context, causing the watch to come to an end
			cib.On("Get", tc.getParams).Return(tc.getResults[len(tc.getResults)-1], nil).
				Run(func(args mock.Arguments) {
					cancel()
				})

			ticker, tick := fakeTicker(ctx)
			done := make(chan error, 1)

			// Start the subscriber, which is controlled by our fake ticker
			go func() {
				// Use a debug logger to see where things go wrong
				logger := logrus.StandardLogger()
				logger.Level = logrus.DebugLevel

				s := subscriber{
					cib:       cib,
					logger:    logger,
					nodes:     tc.nodes,
					handlers:  tc.handlers,
					newTicker: func() *time.Ticker { return ticker },
				}

				s.Start(ctx)
				done <- nil
			}()

			// Wait for the subscriber to conclude, or for us to timeout
			require.Nil(t, func() error {
				timeout := time.After(time.Second)

				for {
					select {
					case <-done:
						return nil
					case <-timeout:
						return errors.New("timed out")
					default:
						tick()
					}
				}
			}())

			// Verify all our handlers have received the calls we expected them to
			for _, handler := range tc.handlers {
				h, ok := handler.(*fakeHandler)
				require.True(t, ok)

				h.AssertExpectations(t)
			}
		})
	}
}
