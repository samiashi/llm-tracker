// jsdom has <dialog> but none of its methods, so expanding a card would throw.
// This covers what the dashboard uses: the open state and the close event. It
// cannot model the top layer, inertness or Esc, which only a browser has.
if (!("showModal" in HTMLDialogElement.prototype)) {
  Object.assign(HTMLDialogElement.prototype, {
    showModal(this: HTMLDialogElement) {
      this.open = true;
    },
    close(this: HTMLDialogElement, returnValue?: string) {
      if (!this.open) return;
      this.open = false;
      if (returnValue !== undefined) this.returnValue = returnValue;
      // Queued, as in a browser, not dispatched from inside close().
      setTimeout(() => this.dispatchEvent(new Event("close")));
    },
  });
}
