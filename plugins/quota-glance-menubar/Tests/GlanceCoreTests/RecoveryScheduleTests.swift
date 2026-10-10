import Foundation
import XCTest
@testable import GlanceCore

final class RecoveryScheduleTests: XCTestCase {
    private func steps(_ count: Int, _ next: () -> TimeInterval?) -> [TimeInterval?] {
        (0..<count).map { _ in next() }
    }

    func testPageWithNoAnswerRetriesWithinSecondsThenSlowsDown() {
        var schedule = RecoverySchedule()
        XCTAssertEqual(steps(12) { schedule.pageFailed(.network) },
                       [2, 5, 10, 20, 40, 60, 120, 300, 600, 900, 900, 900])
        XCTAssertEqual(schedule.pending, .page)
    }

    // An HTTP error used to reload the page every minute, forever.
    func testHTTPErrorsAndStoppedPagesUseOnlyTheSlowSteps() {
        var schedule = RecoverySchedule()
        XCTAssertEqual(steps(7) { schedule.pageFailed(.server) }, [60, 120, 300, 600, 900, 900, 900])
    }

    func testFinishedLoadForgetsPageFailures() {
        var schedule = RecoverySchedule()
        _ = steps(3) { schedule.pageFailed(.network) }
        schedule.pageStarted()
        XCTAssertNil(schedule.pending)
        schedule.pageLoaded()
        XCTAssertEqual(schedule.pageFailed(.server), 60)
    }

    func testNetworkWakeOrUserStartsOverAndBringsAWaitingRetryForward() {
        var schedule = RecoverySchedule()
        XCTAssertNil(schedule.restart(), "Nothing waits, so nothing is brought forward")
        _ = steps(6) { schedule.pageFailed(.server) }
        XCTAssertEqual(schedule.restart(), 2)
        XCTAssertEqual(schedule.pending, .page)
        XCTAssertEqual(schedule.pageFailed(.server), 60)
        XCTAssertEqual(schedule.take(), .page)
        XCTAssertNil(schedule.pending)
        XCTAssertNil(schedule.restart())
    }

    // After a wake the first poll often fails before Wi-Fi or the VPN is back;
    // it used to wait for the next 60-second tick.
    func testPollWithNoAnswerRetriesOnTheQuickStepsThenLeavesItToTheClock() {
        var schedule = RecoverySchedule()
        XCTAssertEqual(steps(7) { schedule.pollFailed() }, [2, 5, 10, 20, 40, nil, nil])
        schedule.received()
        XCTAssertEqual(schedule.pollFailed(), 2)
        schedule.pollStarted()
        XCTAssertNil(schedule.pending)
    }

    func testDuePageRetryWinsOverAPollRetry() {
        var schedule = RecoverySchedule()
        XCTAssertEqual(schedule.pollFailed(), 2)
        XCTAssertEqual(schedule.pageFailed(.network), 2)
        XCTAssertEqual(schedule.pending, .page)
        XCTAssertNil(schedule.pollFailed(), "A poll retry must not replace the page's")
        schedule.received()
        XCTAssertEqual(schedule.pending, .page, "A reading from the old document does not fix a failed load")
    }

    /// Loads the page and lets it go without a reading until it is due again,
    /// then runs that load. Returns the delay it waited for.
    private func stall(_ schedule: inout RecoverySchedule) -> TimeInterval? {
        schedule.pageLoaded()
        XCTAssertEqual(steps(RecoverySchedule.stalledTicks - 1) { schedule.ticked() }, [nil, nil])
        let delay = schedule.ticked()
        if schedule.take() == .page { schedule.pageStarted() }
        return delay
    }

    func testLoadedPageWithoutAReadingIsLoadedAgainOnTheSlowSteps() {
        var schedule = RecoverySchedule()
        XCTAssertNil(schedule.ticked(), "No page has loaded")
        XCTAssertEqual(steps(RecoverySchedule.stalledReloads) { stall(&schedule) }, [60, 120, 300],
                       "A finished reload must not reset the stall steps")
    }

    // A dashboard left signed out never reads anything, and was downloaded
    // again every 15 minutes for as long as the app ran.
    func testPageThatNeverReadsStopsBeingLoadedUntilTheStepsStartOver() {
        var schedule = RecoverySchedule()
        _ = steps(RecoverySchedule.stalledReloads) { stall(&schedule) }
        schedule.pageLoaded()
        XCTAssertEqual(steps(30) { schedule.ticked() }, Array(repeating: nil, count: 30))
        XCTAssertNil(schedule.pending)
        XCTAssertNil(schedule.restart(), "Nothing waits, so nothing is brought forward")
        XCTAssertEqual(steps(RecoverySchedule.stalledTicks) { schedule.ticked() }, [nil, nil, 60],
                       "The network, a wake or the user starts the stall steps over")
        XCTAssertEqual(schedule.take(), .page)
        schedule.pageStarted()
        XCTAssertEqual(steps(RecoverySchedule.stalledReloads - 1) { stall(&schedule) }, [120, 300])
        XCTAssertNil(stall(&schedule))
    }

    func testAReadingLetsAStoppedPageBeLoadedAgain() {
        var schedule = RecoverySchedule()
        _ = steps(RecoverySchedule.stalledReloads) { stall(&schedule) }
        schedule.pageLoaded()
        schedule.received()
        XCTAssertEqual(steps(RecoverySchedule.stalledReloads) { stall(&schedule) }, [60, 120, 300])
        XCTAssertNil(stall(&schedule))
    }

    func testStoppedStallLoadsLeaveFailedPagesAndPollsRetrying() {
        var schedule = RecoverySchedule()
        _ = steps(RecoverySchedule.stalledReloads) { stall(&schedule) }
        XCTAssertEqual(schedule.pollFailed(), 2)
        XCTAssertEqual(schedule.pageFailed(.network), 2)
        XCTAssertEqual(schedule.take(), .page)
    }

    func testAReadingEndsTheStallCount() {
        var schedule = RecoverySchedule()
        schedule.pageLoaded()
        _ = steps(RecoverySchedule.stalledTicks) { schedule.ticked() }
        schedule.pageStarted()
        schedule.pageLoaded()
        schedule.received()
        XCTAssertEqual(steps(RecoverySchedule.stalledTicks + 1) { schedule.ticked() }, [nil, nil, nil, nil])
        schedule.pageLoaded()
        XCTAssertEqual(steps(RecoverySchedule.stalledTicks) { schedule.ticked() }.last, 60)
    }

    func testStalledPageIsNotRescheduledWhileItsRetryWaits() {
        var schedule = RecoverySchedule()
        schedule.pageLoaded()
        XCTAssertEqual(steps(RecoverySchedule.stalledTicks) { schedule.ticked() }.last, 60)
        XCTAssertEqual(steps(5) { schedule.ticked() }, [nil, nil, nil, nil, nil])
        // The retry ran but found the page in use, so it did not load it.
        XCTAssertEqual(schedule.take(), .page)
        XCTAssertEqual(steps(RecoverySchedule.stalledTicks) { schedule.ticked() }, [nil, nil, 120])
    }

    func testFailedLoadStopsCountingTicksUntilAPageLoads() {
        var schedule = RecoverySchedule()
        schedule.pageLoaded()
        _ = schedule.pageFailed(.server)
        _ = schedule.take()
        XCTAssertEqual(steps(RecoverySchedule.stalledTicks + 1) { schedule.ticked() }, [nil, nil, nil, nil])
    }

    // The dashboard never presents a console key again after a request with no
    // answer until the reader asks, and a reload would present it.
    func testUnansweredConsoleRequestIsNeverRetriedByReloading() {
        var schedule = RecoverySchedule()
        schedule.pageLoaded()
        schedule.consoleUnanswered()
        XCTAssertEqual(steps(10) { schedule.ticked() }, Array(repeating: nil, count: 10))
        XCTAssertNil(schedule.restart(), "Neither a wake nor the network coming back lifts the hold")
        XCTAssertNil(schedule.ticked())
        schedule.pageStarted()
        schedule.pageLoaded()
        XCTAssertEqual(steps(RecoverySchedule.stalledTicks) { schedule.ticked() }.last, 60,
                       "A page loaded for another reason starts with a clean slate")
    }

    // A VPN or Tailscale changing interfaces reports a usable path again, and
    // each report used to start the steps over, keeping an HTTP error or a
    // stopped page near the 2-second step.
    func testOnlyAUsablePathAfterAnUnusableOneIsTheNetworkComingBack() {
        var path = RecoverySchedule.NetworkPath()
        XCTAssertFalse(path.update(usable: true), "The first update reports the path as it already was")
        XCTAssertFalse(path.update(usable: true), "Churn between usable paths")
        XCTAssertFalse(path.update(usable: false))
        XCTAssertFalse(path.update(usable: false))
        XCTAssertTrue(path.update(usable: true))
        XCTAssertFalse(path.update(usable: true))

        var offline = RecoverySchedule.NetworkPath()
        XCTAssertFalse(offline.update(usable: false), "Starting offline is not a transition")
        XCTAssertTrue(offline.update(usable: true))
    }

    func testClassifiesPageLoadErrors() {
        for code in [NSURLErrorTimedOut, NSURLErrorCannotFindHost, NSURLErrorCannotConnectToHost,
                     NSURLErrorNetworkConnectionLost, NSURLErrorDNSLookupFailed, NSURLErrorNotConnectedToInternet] {
            XCTAssertEqual(RecoverySchedule.Failure(loadErrorDomain: NSURLErrorDomain, code: code), .network, "\(code)")
        }
        for code in [NSURLErrorBadServerResponse, NSURLErrorSecureConnectionFailed, NSURLErrorServerCertificateUntrusted] {
            XCTAssertEqual(RecoverySchedule.Failure(loadErrorDomain: NSURLErrorDomain, code: code), .server, "\(code)")
        }
        XCTAssertEqual(RecoverySchedule.Failure(loadErrorDomain: "WKErrorDomain", code: 2), .server)
        XCTAssertNil(RecoverySchedule.Failure(loadErrorDomain: NSURLErrorDomain, code: NSURLErrorCancelled))
        XCTAssertNil(RecoverySchedule.Failure(loadErrorDomain: "WebKitErrorDomain", code: 102),
                     "Cancelling an HTTP error response must not count as a second failure")
    }
}
