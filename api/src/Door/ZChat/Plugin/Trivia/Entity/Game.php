<?php

namespace App\Door\ZChat\Plugin\Trivia\Entity;

use App\Door\ZChat\Entity\Room;
use App\Door\ZChat\Plugin\Trivia\Entity\Enum\GameStatus;
use App\Door\ZChat\Plugin\Trivia\Repository\GameRepository;
use Doctrine\Common\Collections\ArrayCollection;
use Doctrine\Common\Collections\Collection;
use Doctrine\ORM\Mapping as ORM;
use Symfony\Bridge\Doctrine\Types\UuidType;
use Symfony\Component\Uid\Uuid;

#[ORM\Entity(repositoryClass: GameRepository::class)]
#[ORM\Table(name: 'zchat_trivia_game')]
#[ORM\Index(columns: ['room_id', 'status'], name: 'idx_zchat_trivia_game_room')]
class Game
{
    #[ORM\Id]
    #[ORM\Column(type: UuidType::NAME, unique: true)]
    private Uuid $id;

    #[ORM\ManyToOne]
    #[ORM\JoinColumn(nullable: false)]
    private Room $room;

    #[ORM\Column(type: 'string', length: 20, enumType: GameStatus::class)]
    private GameStatus $status = GameStatus::WAITING;

    #[ORM\Column]
    private int $currentRound = 0;

    #[ORM\Column]
    private int $maxRounds = 10;

    #[ORM\ManyToOne]
    #[ORM\JoinColumn(nullable: true)]
    private ?Question $currentQuestion = null;

    #[ORM\Column(nullable: true)]
    private ?\DateTimeImmutable $questionStartedAt = null;

    #[ORM\Column]
    private int $questionTimeout = 90;

    #[ORM\Column]
    private \DateTimeImmutable $createdAt;

    #[ORM\Column(nullable: true)]
    private ?\DateTimeImmutable $endedAt = null;

    /** @var Collection<int, Team> */
    #[ORM\OneToMany(targetEntity: Team::class, mappedBy: 'game', orphanRemoval: true)]
    private Collection $teams;

    /** @var Collection<int, Player> */
    #[ORM\OneToMany(targetEntity: Player::class, mappedBy: 'game', orphanRemoval: true)]
    private Collection $players;

    public function __construct()
    {
        $this->id = Uuid::v7();
        $this->createdAt = new \DateTimeImmutable();
        $this->teams = new ArrayCollection();
        $this->players = new ArrayCollection();
    }

    public function getId(): Uuid
    {
        return $this->id;
    }

    public function getRoom(): Room
    {
        return $this->room;
    }

    public function setRoom(Room $room): static
    {
        $this->room = $room;
        return $this;
    }

    public function getStatus(): GameStatus
    {
        return $this->status;
    }

    public function setStatus(GameStatus $status): static
    {
        $this->status = $status;
        return $this;
    }

    public function getCurrentRound(): int
    {
        return $this->currentRound;
    }

    public function nextRound(): static
    {
        $this->currentRound++;
        return $this;
    }

    public function getMaxRounds(): int
    {
        return $this->maxRounds;
    }

    public function setMaxRounds(int $maxRounds): static
    {
        $this->maxRounds = $maxRounds;
        return $this;
    }

    public function getCurrentQuestion(): ?Question
    {
        return $this->currentQuestion;
    }

    public function setCurrentQuestion(?Question $currentQuestion): static
    {
        $this->currentQuestion = $currentQuestion;
        if ($currentQuestion !== null) {
            $this->questionStartedAt = new \DateTimeImmutable();
        } else {
            $this->questionStartedAt = null;
        }
        return $this;
    }

    public function getQuestionStartedAt(): ?\DateTimeImmutable
    {
        return $this->questionStartedAt;
    }

    public function getQuestionTimeout(): int
    {
        return $this->questionTimeout;
    }

    public function setQuestionTimeout(int $questionTimeout): static
    {
        $this->questionTimeout = $questionTimeout;
        return $this;
    }

    public function isQuestionExpired(): bool
    {
        if ($this->questionStartedAt === null) {
            return true;
        }
        $elapsed = (new \DateTimeImmutable())->getTimestamp() - $this->questionStartedAt->getTimestamp();
        return $elapsed >= $this->questionTimeout;
    }

    public function getSecondsRemaining(): int
    {
        if ($this->questionStartedAt === null) {
            return 0;
        }
        $elapsed = (new \DateTimeImmutable())->getTimestamp() - $this->questionStartedAt->getTimestamp();
        return max(0, $this->questionTimeout - $elapsed);
    }

    public function getCreatedAt(): \DateTimeImmutable
    {
        return $this->createdAt;
    }

    public function getEndedAt(): ?\DateTimeImmutable
    {
        return $this->endedAt;
    }

    public function end(): static
    {
        $this->status = GameStatus::FINISHED;
        $this->endedAt = new \DateTimeImmutable();
        return $this;
    }

    public function isActive(): bool
    {
        return $this->status !== GameStatus::FINISHED;
    }

    public function isLastRound(): bool
    {
        return $this->currentRound >= $this->maxRounds;
    }

    /** @return Collection<int, Team> */
    public function getTeams(): Collection
    {
        return $this->teams;
    }

    public function addTeam(Team $team): static
    {
        if (!$this->teams->contains($team)) {
            $this->teams->add($team);
            $team->setGame($this);
        }
        return $this;
    }

    /** @return Collection<int, Player> */
    public function getPlayers(): Collection
    {
        return $this->players;
    }

    public function addPlayer(Player $player): static
    {
        if (!$this->players->contains($player)) {
            $this->players->add($player);
            $player->setGame($this);
        }
        return $this;
    }
}
