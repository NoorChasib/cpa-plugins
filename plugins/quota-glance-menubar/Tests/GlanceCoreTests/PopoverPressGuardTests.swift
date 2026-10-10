import Foundation
import XCTest
@testable import GlanceCore

final class PopoverPressGuardTests: XCTestCase {
    // Event timestamps and system uptime share one clock; any point on it will do.
    private let press: TimeInterval = 1_000

    private func closed(at time: TimeInterval, pointerOnItem: Bool) -> PopoverPressGuard {
        var pressGuard = PopoverPressGuard()
        pressGuard.recordClose(at: time, pointerOnItem: pointerOnItem)
        return pressGuard
    }

    func testNothingClosedSwallowsNothing() {
        let pressGuard = PopoverPressGuard()
        XCTAssertFalse(pressGuard.pressClosedPopover(pressedAt: press, now: press))
        XCTAssertFalse(pressGuard.pressClosedPopover(pressedAt: nil, now: press))
    }

    // The transient behaviour closed the popover in answer to this same press.
    func testCloseBegunWithinHalfASecondAfterThePressIsSwallowed() {
        for delay in [0, 0.09, 0.499] {
            let pressGuard = closed(at: press + delay, pointerOnItem: false)
            XCTAssertTrue(pressGuard.pressClosedPopover(pressedAt: press, now: press + delay + 0.01), "\(delay)")
        }
        let late = closed(at: press + 0.5, pointerOnItem: false)
        XCTAssertFalse(late.pressClosedPopover(pressedAt: press, now: press + 0.51), "Half a second is already too late")
    }

    // On macOS 27 the click can reach the popover about 90 ms before the button.
    func testCloseJustBeforeThePressWithThePointerOnTheItemIsSwallowed() {
        let pressGuard = closed(at: press - 0.09, pointerOnItem: true)
        XCTAssertTrue(pressGuard.pressClosedPopover(pressedAt: press, now: press + 0.01))
        // Only the close's age counts here, not how far it lies from the press.
        let farFromThePress = closed(at: press + 0.6, pointerOnItem: true)
        XCTAssertTrue(farFromThePress.pressClosedPopover(pressedAt: press, now: press + 0.7))
    }

    // A click elsewhere closed the popover; this press is a new one and opens it.
    func testCloseBeforeThePressWithThePointerElsewhereIsNotSwallowed() {
        let pressGuard = closed(at: press - 0.09, pointerOnItem: false)
        XCTAssertFalse(pressGuard.pressClosedPopover(pressedAt: press, now: press + 0.01))
    }

    func testCloseHalfASecondOldOrOlderIsNotSwallowed() {
        let old = closed(at: press - 0.6, pointerOnItem: true)
        XCTAssertFalse(old.pressClosedPopover(pressedAt: press, now: press + 0.01))
        // Measured to now: a close 0.3 s before a press handled 0.3 s late is too old.
        let handledLate = closed(at: press - 0.3, pointerOnItem: true)
        XCTAssertFalse(handledLate.pressClosedPopover(pressedAt: press, now: press + 0.3))
        let edge = closed(at: press - 0.25, pointerOnItem: true)
        XCTAssertFalse(edge.pressClosedPopover(pressedAt: press, now: press + 0.25), "Exactly half a second")
    }

    // Escape or Settings closed it, or a press already handled its own close:
    // a click straight after must open the popover again.
    func testClearedCloseSwallowsNothing() {
        var pressGuard = closed(at: press, pointerOnItem: true)
        pressGuard.clear()
        XCTAssertEqual(pressGuard, PopoverPressGuard())
        XCTAssertFalse(pressGuard.pressClosedPopover(pressedAt: press, now: press + 0.01))
        XCTAssertFalse(pressGuard.pressClosedPopover(pressedAt: nil, now: press + 0.01))
    }

    // Without an event only the pointer rule can apply.
    func testPressWithoutAnEventFallsBackToThePointer() {
        let onItem = closed(at: press, pointerOnItem: true)
        XCTAssertTrue(onItem.pressClosedPopover(pressedAt: nil, now: press + 0.2))
        XCTAssertFalse(onItem.pressClosedPopover(pressedAt: nil, now: press + 0.5))
        let elsewhere = closed(at: press, pointerOnItem: false)
        XCTAssertFalse(elsewhere.pressClosedPopover(pressedAt: nil, now: press + 0.01))
    }
}
