import { useEffect, useRef } from "react";

/**
 * A confirmation dialog built on the native <dialog> element.
 *
 * Native rather than a hand-rolled overlay because showModal() brings focus
 * trapping, Escape-to-dismiss, inertness of the rest of the page, and the
 * top-layer stacking for free — all things a div-with-a-backdrop has to
 * reimplement and usually gets wrong for keyboard and screen-reader users.
 */
export default function ConfirmDialog({
  open,
  title,
  message,
  confirmLabel = "Confirm",
  onConfirm,
  onCancel,
}: {
  open: boolean;
  title: string;
  message: string;
  confirmLabel?: string;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const ref = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (open && !el.open) el.showModal();
    if (!open && el.open) el.close();
  }, [open]);

  return (
    <dialog
      ref={ref}
      className="confirm"
      // Escape and the backdrop both cancel, so the dialog cannot be dismissed
      // into an ambiguous state where the caller never hears back.
      onCancel={(e) => {
        e.preventDefault();
        onCancel();
      }}
      onClick={(e) => {
        if (e.target === ref.current) onCancel();
      }}
    >
      <h2>{title}</h2>
      <p>{message}</p>
      <div className="confirm-actions">
        <button onClick={onCancel}>Cancel</button>
        <button className="primary" onClick={onConfirm} autoFocus>
          {confirmLabel}
        </button>
      </div>
    </dialog>
  );
}
