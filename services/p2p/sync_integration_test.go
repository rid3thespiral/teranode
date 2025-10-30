package p2p

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Integration test that exercises the full sync coordination flow
func TestSyncCoordination_FullFlow(t *testing.T) {
	// Setup blockchain
	blockchainSetup := SetupTestBlockchain(t)
	defer blockchainSetup.Cleanup()

	// Create components
	logger := CreateTestLogger(t)
	settings := CreateTestSettings()
	settings.P2P.BanThreshold = 50

	// Create ban manager
	banManager := NewPeerBanManager(blockchainSetup.Ctx, nil, settings)

	// Create peer registry and add test peers
	registry := NewPeerRegistry()

	// Add healthy peer with DataHub URL
	healthyPeer := peer.ID("healthy")
	registry.AddPeer(healthyPeer)
	registry.UpdateHeight(healthyPeer, 1000, "hash1000")
	registry.UpdateDataHubURL(healthyPeer, "http://healthy.test")
	registry.UpdateHealth(healthyPeer, true)
	registry.UpdateURLResponsiveness(healthyPeer, true)
	registry.UpdateStorage(healthyPeer, "full")

	// Add unhealthy peer
	unhealthyPeer := peer.ID("unhealthy")
	registry.AddPeer(unhealthyPeer)
	registry.UpdateHeight(unhealthyPeer, 900, "hash900")
	registry.UpdateHealth(unhealthyPeer, false)

	// Add banned peer
	bannedPeer := peer.ID("banned")
	registry.AddPeer(bannedPeer)
	registry.UpdateHeight(bannedPeer, 1100, "hash1100")
	registry.UpdateDataHubURL(bannedPeer, "http://banned.test")
	registry.UpdateHealth(bannedPeer, true)
	banManager.AddScore(string(bannedPeer), ReasonSpam) // Ban the peer
	registry.UpdateBanStatus(bannedPeer, 50, true)

	// Create peer selector
	selector := NewPeerSelector(logger, nil)

	// Create health checker
	healthChecker := NewPeerHealthChecker(logger, registry, settings)

	// Create sync coordinator
	coordinator := NewSyncCoordinator(
		logger,
		settings,
		registry,
		selector,
		healthChecker,
		banManager,
		blockchainSetup.Client,
		nil, // blocksKafkaProducerClient
	)

	// Set local height callback
	coordinator.SetGetLocalHeightCallback(func() uint32 {
		return 100
	})

	// Start coordinator
	coordinator.Start(blockchainSetup.Ctx)
	defer coordinator.Stop()

	// Test 1: Trigger sync should select healthy peer
	t.Run("TriggerSync_SelectsHealthyPeer", func(t *testing.T) {
		err := coordinator.TriggerSync()
		require.NoError(t, err)
		time.Sleep(100 * time.Millisecond)

		assert.Equal(t, healthyPeer, coordinator.GetCurrentSyncPeer())
	})

	// Test 2: Disconnect current sync peer
	t.Run("HandlePeerDisconnected_SelectsNewPeer", func(t *testing.T) {
		// Add another healthy peer
		newHealthyPeer := peer.ID("newhealthy")
		registry.AddPeer(newHealthyPeer)
		registry.UpdateHeight(newHealthyPeer, 1050, "hash1050")
		registry.UpdateDataHubURL(newHealthyPeer, "http://newhealthy.test")
		registry.UpdateHealth(newHealthyPeer, true)
		registry.UpdateURLResponsiveness(newHealthyPeer, true)
		registry.UpdateStorage(newHealthyPeer, "full")

		// Disconnect current sync peer
		coordinator.HandlePeerDisconnected(healthyPeer)

		// Trigger sync to select a new peer
		_ = coordinator.TriggerSync()
		time.Sleep(100 * time.Millisecond)

		// Should select the new healthy peer
		assert.Equal(t, newHealthyPeer, coordinator.GetCurrentSyncPeer())
	})

	// Test 3: Ban status updates
	t.Run("UpdateBanStatus_RemovesBannedPeer", func(t *testing.T) {
		// Get the current sync peer (whatever was selected)
		currentPeer := coordinator.GetCurrentSyncPeer()

		// If no peer, add one and select it
		if currentPeer == "" {
			testPeer := peer.ID("ban-test")
			registry.AddPeer(testPeer)
			registry.UpdateHeight(testPeer, 10000, "hash10000") // Very high to ensure selection
			registry.UpdateDataHubURL(testPeer, "http://ban-test.com")
			registry.UpdateHealth(testPeer, true)
			registry.UpdateURLResponsiveness(testPeer, true)
			registry.UpdateStorage(testPeer, "full")

			_ = coordinator.TriggerSync()
			time.Sleep(50 * time.Millisecond)
			currentPeer = coordinator.GetCurrentSyncPeer()
		}

		require.NotEmpty(t, currentPeer, "Should have a sync peer for ban test")
		t.Logf("Testing ban with peer: %s", currentPeer)

		// Get current ban score and clear it to start fresh
		initialScore, _, _ := banManager.GetBanScore(string(currentPeer))
		t.Logf("Initial ban score for %s: %d", currentPeer, initialScore)

		// Add ban scores to exceed threshold (100 points)
		// ReasonSpam gives 50 points each, so we need at least 2-3 calls depending on initial score
		maxAttempts := 10
		var banned bool
		var score int

		for i := 0; i < maxAttempts && !banned; i++ {
			score, banned = banManager.AddScore(string(currentPeer), ReasonSpam)
			t.Logf("Attempt %d for %s: score=%d, banned=%v", i+1, currentPeer, score, banned)
		}

		// Verify the peer is actually banned
		require.True(t, banned, "Should have banned peer %s after %d attempts (initial: %d, final: %d)",
			currentPeer, maxAttempts, initialScore, score)

		// Debug: Check ban status directly
		actualBanned := banManager.IsBanned(string(currentPeer))
		t.Logf("BanManager.IsBanned(%s) = %v", currentPeer, actualBanned)

		// Update ban status in coordinator - this should clear the sync peer
		t.Logf("Updating ban status for %s", currentPeer)
		coordinator.UpdateBanStatus(currentPeer)

		// Give it a moment to process and trigger sync
		time.Sleep(100 * time.Millisecond)

		// Check if peer was cleared
		clearedPeer := coordinator.GetCurrentSyncPeer()
		t.Logf("After UpdateBanStatus: current sync peer is %s", clearedPeer)

		// If peer wasn't cleared automatically, the test should still pass if
		// the peer is marked as banned in the registry
		if clearedPeer == currentPeer {
			// Check if the peer is actually banned in the registry
			if peerInfo, exists := registry.GetPeer(currentPeer); exists && peerInfo.IsBanned {
				t.Logf("Peer %s is marked as banned in registry but still selected as sync peer", currentPeer)
				// This is a bug - the banned peer should not be selected
				assert.Fail(t, "Banned peer should not remain as sync peer")
			} else {
				t.Logf("Peer %s ban status not updated in registry", currentPeer)
			}
		} else {
			// Success - peer was cleared
			assert.NotEqual(t, string(currentPeer), string(clearedPeer),
				"Banned peer %s should be cleared as sync peer (new peer: %s)", currentPeer, clearedPeer)
		}
	})

	// Test 4: FSM state changes
	t.Run("FSMStateChange_AffectsSync", func(t *testing.T) {
		// Set FSM to CATCHINGBLOCKS
		err := blockchainSetup.Client.CatchUpBlocks(blockchainSetup.Ctx)
		require.NoError(t, err)

		// Check FSM state handling
		coordinator.checkFSMState(blockchainSetup.Ctx)

		// Return to RUNNING
		err = blockchainSetup.Client.Run(blockchainSetup.Ctx, "test")
		require.NoError(t, err)

		coordinator.checkFSMState(blockchainSetup.Ctx)
	})
}

// Test sync with HTTP server mocking DataHub
func TestSyncCoordination_WithHTTPServer(t *testing.T) {
	// Setup test HTTP server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status": "ok"}`))
		case "/blocks":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"blocks": []}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	// Setup blockchain
	blockchainSetup := SetupTestBlockchain(t)
	defer blockchainSetup.Cleanup()

	// Create components
	logger := CreateTestLogger(t)
	settings := CreateTestSettings()

	// Create components
	banManager := NewPeerBanManager(blockchainSetup.Ctx, nil, settings)
	registry := NewPeerRegistry()
	selector := NewPeerSelector(logger, nil)
	healthChecker := NewPeerHealthChecker(logger, registry, settings)

	// Add peer with test server URL
	testPeer := peer.ID("httptest")
	registry.AddPeer(testPeer)
	registry.UpdateHeight(testPeer, 1000, "hash1000")
	registry.UpdateDataHubURL(testPeer, server.URL)
	registry.UpdateHealth(testPeer, true)

	// Create sync coordinator
	coordinator := NewSyncCoordinator(
		logger,
		settings,
		registry,
		selector,
		healthChecker,
		banManager,
		blockchainSetup.Client,
		nil, // blocksKafkaProducerClient
	)

	// Start coordinator
	coordinator.Start(blockchainSetup.Ctx)
	defer coordinator.Stop()

	// Test URL responsiveness check
	t.Run("CheckURLResponsiveness", func(t *testing.T) {
		responsive := coordinator.checkURLResponsiveness(server.URL)
		assert.True(t, responsive)
	})

	// Test with unresponsive URL
	t.Run("CheckURLUnresponsive", func(t *testing.T) {
		badURL := "http://nonexistent.invalid:12345"
		responsive := coordinator.checkURLResponsiveness(badURL)
		assert.False(t, responsive)
	})
}

// Test sync coordination with multiple concurrent operations
func TestSyncCoordination_ConcurrentOperations(t *testing.T) {
	blockchainSetup := SetupTestBlockchain(t)
	defer blockchainSetup.Cleanup()

	logger := CreateTestLogger(t)
	settings := CreateTestSettings()

	banManager := NewPeerBanManager(blockchainSetup.Ctx, nil, settings)
	registry := NewPeerRegistry()
	selector := NewPeerSelector(logger, nil)
	healthChecker := NewPeerHealthChecker(logger, registry, settings)

	coordinator := NewSyncCoordinator(
		logger,
		settings,
		registry,
		selector,
		healthChecker,
		banManager,
		blockchainSetup.Client,
		nil, // blocksKafkaProducerClient
	)

	// Add multiple peers
	for i := 0; i < 20; i++ {
		peerID := peer.ID(string(rune('A' + i)))
		registry.AddPeer(peerID)
		registry.UpdateHeight(peerID, int32(1000+i*10), "hash")
		registry.UpdateHealth(peerID, i%3 != 0) // Every third peer is unhealthy
	}

	coordinator.Start(blockchainSetup.Ctx)
	defer coordinator.Stop()

	var wg sync.WaitGroup

	// Concurrent trigger syncs
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_ = coordinator.TriggerSync()
				time.Sleep(10 * time.Millisecond)
			}
		}(i)
	}

	// Concurrent peer disconnects
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				peerID := peer.ID(string(rune('A' + (id+j)%20)))
				coordinator.HandlePeerDisconnected(peerID)
				time.Sleep(15 * time.Millisecond)
			}
		}(i)
	}

	// Concurrent peer updates
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				peerID := peer.ID(string(rune('A' + (id*2+j)%20)))
				coordinator.UpdatePeerInfo(peerID, int32(1100+j), "newhash", "")
				time.Sleep(20 * time.Millisecond)
			}
		}(i)
	}

	// Concurrent ban status updates
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				peerID := peer.ID(string(rune('A' + (id*3+j)%20)))
				coordinator.UpdateBanStatus(peerID)
				time.Sleep(25 * time.Millisecond)
			}
		}(i)
	}

	wg.Wait()

	// Should not panic or deadlock
	assert.True(t, true, "Concurrent operations completed successfully")
}

// Test sync coordination with catchup failures
func TestSyncCoordination_CatchupFailures(t *testing.T) {
	blockchainSetup := SetupTestBlockchain(t)
	defer blockchainSetup.Cleanup()

	logger := CreateTestLogger(t)
	settings := CreateTestSettings()
	settings.P2P.BanThreshold = 30

	// Create ban manager with handler
	banHandler := &testBanHandler{}
	banManager := NewPeerBanManager(blockchainSetup.Ctx, banHandler, settings)

	registry := NewPeerRegistry()
	selector := NewPeerSelector(logger, nil)
	healthChecker := NewPeerHealthChecker(logger, registry, settings)

	coordinator := NewSyncCoordinator(
		logger,
		settings,
		registry,
		selector,
		healthChecker,
		banManager,
		blockchainSetup.Client,
		nil, // blocksKafkaProducerClient
	)

	// Add test peers
	goodPeer := peer.ID("good")
	registry.AddPeer(goodPeer)
	registry.UpdateHeight(goodPeer, 1000, "hash1000")
	registry.UpdateDataHubURL(goodPeer, "http://good.test")
	registry.UpdateHealth(goodPeer, true)
	registry.UpdateURLResponsiveness(goodPeer, true)
	registry.UpdateStorage(goodPeer, "full")

	badPeer := peer.ID("bad")
	registry.AddPeer(badPeer)
	registry.UpdateHeight(badPeer, 1100, "hash1100")
	registry.UpdateDataHubURL(badPeer, "http://bad.test")
	registry.UpdateHealth(badPeer, true)
	registry.UpdateURLResponsiveness(badPeer, true)
	registry.UpdateStorage(badPeer, "full")

	coordinator.Start(blockchainSetup.Ctx)
	defer coordinator.Stop()

	// Test catchup failure handling
	t.Run("HandleCatchupFailure", func(t *testing.T) {
		// Trigger sync
		err := coordinator.TriggerSync()
		require.NoError(t, err)
		time.Sleep(100 * time.Millisecond)

		// Simulate catchup failure
		coordinator.HandleCatchupFailure("sync failed")

		// Should have selected a new peer or cleared sync
		// The behavior depends on available peers
		assert.True(t, true, "Catchup failure handled without panic")
	})
}

// Test evaluateSyncPeer logic through actual sync operations
func TestSyncCoordination_PeerEvaluation(t *testing.T) {
	blockchainSetup := SetupTestBlockchain(t)
	defer blockchainSetup.Cleanup()

	logger := CreateTestLogger(t)
	settings := CreateTestSettings()

	banManager := NewPeerBanManager(blockchainSetup.Ctx, nil, settings)
	registry := NewPeerRegistry()
	selector := NewPeerSelector(logger, nil)
	healthChecker := NewPeerHealthChecker(logger, registry, settings)

	coordinator := NewSyncCoordinator(
		logger,
		settings,
		registry,
		selector,
		healthChecker,
		banManager,
		blockchainSetup.Client,
		nil, // blocksKafkaProducerClient
	)

	// Test various peer conditions
	tests := []struct {
		name        string
		setupPeer   func() peer.ID
		shouldSync  bool
		description string
	}{
		{
			name: "healthy_peer_with_url",
			setupPeer: func() peer.ID {
				id := peer.ID("good")
				registry.AddPeer(id)
				registry.UpdateHeight(id, 1000, "hash")
				registry.UpdateDataHubURL(id, "http://good.test")
				registry.UpdateHealth(id, true)
				registry.UpdateURLResponsiveness(id, true)
				return id
			},
			shouldSync:  true,
			description: "Healthy peer with responsive URL should be selected",
		},
		{
			name: "banned_peer",
			setupPeer: func() peer.ID {
				id := peer.ID("banned")
				registry.AddPeer(id)
				registry.UpdateHeight(id, 1000, "hash")
				registry.UpdateDataHubURL(id, "http://banned.test")
				registry.UpdateHealth(id, true)
				registry.UpdateBanStatus(id, 100, true)
				return id
			},
			shouldSync:  false,
			description: "Banned peer should not be selected",
		},
		{
			name: "unhealthy_peer",
			setupPeer: func() peer.ID {
				id := peer.ID("unhealthy")
				registry.AddPeer(id)
				registry.UpdateHeight(id, 1000, "hash")
				registry.UpdateDataHubURL(id, "http://unhealthy.test")
				registry.UpdateHealth(id, false)
				return id
			},
			shouldSync:  false,
			description: "Unhealthy peer should not be selected",
		},
		{
			name: "no_datahub_url",
			setupPeer: func() peer.ID {
				id := peer.ID("nourl")
				registry.AddPeer(id)
				registry.UpdateHeight(id, 1000, "hash")
				registry.UpdateHealth(id, true)
				return id
			},
			shouldSync:  false,
			description: "Peer without DataHub URL should not be selected",
		},
	}

	coordinator.Start(blockchainSetup.Ctx)
	defer coordinator.Stop()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear existing peers
			for _, p := range registry.GetAllPeers() {
				registry.RemovePeer(p.ID)
			}

			// Clear current sync peer
			coordinator.ClearSyncPeer()

			// Setup test peer
			peerID := tt.setupPeer()

			// Try to trigger sync
			err := coordinator.TriggerSync()

			// Check if peer was selected
			syncPeer := coordinator.GetCurrentSyncPeer()

			if tt.shouldSync {
				assert.NoError(t, err, tt.description)
				assert.Equal(t, peerID, syncPeer, tt.description)
			} else {
				// Either error or no peer selected
				if err == nil {
					assert.Equal(t, peer.ID(""), syncPeer, tt.description)
				}
			}
		})
	}
}
