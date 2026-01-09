use lightning::chain::chaininterface::{ConfirmationTarget, FeeEstimator};

#[derive(Clone, Debug)]
pub struct FixedFeeEstimator {
	pub sat_per_1000_weight: u32,
}

impl Default for FixedFeeEstimator {
	fn default() -> Self {
		Self { sat_per_1000_weight: 253 }
	}
}

impl FeeEstimator for FixedFeeEstimator {
	fn get_est_sat_per_1000_weight(&self, _confirmation_target: ConfirmationTarget) -> u32 {
		self.sat_per_1000_weight
	}
}

